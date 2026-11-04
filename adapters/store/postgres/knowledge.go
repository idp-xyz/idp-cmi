package postgres

import (
	"context"
	"errors"
	"idp-cmi/internal/guard"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
)

const docCols = `d.id::text, s.name, d.uri, CASE WHEN d.erasure_id IS NULL THEN d.title ELSE '[erased]' END, d.version, d.content_sha256, d.byte_size, d.chunk_count, d.status, d.valid_until, d.sensitivity, d.acl_subjects, d.acl_roles,
	d.revoked_at, coalesce(d.revoked_by, ''), coalesce(d.revoked_reason, ''), d.created_by, d.created_at, d.flags, d.revision, ` +
	"(SELECT coalesce(json_agg(json_build_object('type', e.entity_type, 'id', e.entity_id) ORDER BY e.entity_type, e.entity_id), '[]')::text FROM entity_refs e WHERE e.object_kind = 'knowledge' AND e.object_id = d.id)"

func scanDoc(row pgx.Row) (*cmi.Document, error) {
	var d cmi.Document
	var validUntil, revokedAt *time.Time
	var revokedBy, revokedReason, entities string
	if err := row.Scan(&d.ID, &d.Source, &d.URI, &d.Title, &d.Version, &d.ContentSHA256, &d.ByteSize, &d.ChunkCount, &d.Status, &validUntil, &d.Sensitivity, &d.ACL.Subjects, &d.ACL.Roles,
		&revokedAt, &revokedBy, &revokedReason, &d.CreatedBy, &d.CreatedAt, &d.Flags, &d.Revision, &entities); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "document not found")
		}
		return nil, err
	}
	d.Entities = decodeEntities(entities)
	if len(d.Flags) == 0 {
		d.Flags = nil
	}
	if validUntil != nil {
		d.ValidUntil = *validUntil
	}
	if revokedAt != nil {
		d.Revocation = &cmi.Revocation{At: *revokedAt, By: revokedBy, Reason: revokedReason}
	}
	if d.ACL.Subjects == nil {
		d.ACL.Subjects = []string{}
	}
	if d.ACL.Roles == nil {
		d.ACL.Roles = []string{}
	}
	return &d, nil
}

// ChunkSize is the index unit (bytes).
const ChunkSize = 1500

// CreateDocument indexes a document; an existing active version of the same URI is superseded.
func (s *Store) CreateDocument(ctx context.Context, tenant string, in cmi.Document, text, by string) (*cmi.Document, error) {
	if err := cmi.ValidateDocument(&in, text); err != nil {
		return nil, fail("INVALID", "%s", err)
	}
	if in.ACL.Subjects == nil {
		in.ACL.Subjects = []string{}
	}
	if in.ACL.Roles == nil {
		in.ACL.Roles = []string{}
	}
	chunks := cmi.Chunk(text, ChunkSize)
	chunkFlags := make([][]string, len(chunks))
	var docFlags []string
	for i, c := range chunks {
		chunkFlags[i] = guard.Scan(c) // D12
		docFlags = guard.Union(docFlags, chunkFlags[i])
	}
	if docFlags == nil {
		docFlags = []string{}
	}
	in.Entities = cmi.SortEntities(in.Entities)
	var vecs [][]float32
	var model string
	if pol, err := s.GetPolicy(ctx, tenant); err == nil && cmi.Within(in.Sensitivity, pol.ExternalModelCeiling) { // D29
		vecs, model = s.embed(ctx, chunks)
	}
	var out *cmi.Document
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		src, err := getSource(ctx, tx, in.Source)
		if err != nil {
			return fail("CONFLICT", "source %s is not registered", in.Source)
		}
		var version int
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) + 1 FROM knowledge_documents WHERE uri = $1`, in.URI).Scan(&version); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE knowledge_documents SET status = 'superseded', revision = revision + 1 WHERE uri = $1 AND status = 'active'`, in.URI); err != nil {
			return err
		}
		var validUntil any
		if !in.ValidUntil.IsZero() {
			validUntil = in.ValidUntil
		}
		id := NewUUID()
		if _, err := tx.Exec(ctx, `INSERT INTO knowledge_documents (id, tenant_id, source_id, uri, title, version, content_sha256, byte_size, chunk_count, valid_until, sensitivity, acl_subjects, acl_roles, created_by, flags)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
			id, tenant, src.ID, in.URI, in.Title, version, cmi.ContentSHA256(text), len(text), len(chunks), validUntil, in.Sensitivity, in.ACL.Subjects, in.ACL.Roles, by, docFlags); err != nil {
			return err
		}
		b := &pgx.Batch{}
		for i, c := range chunks {
			var vec, vecModel any
			if vecs != nil {
				vec, vecModel = vecs[i], model
			}
			flags := chunkFlags[i]
			if flags == nil {
				flags = []string{}
			}
			b.Queue(`INSERT INTO knowledge_chunks (tenant_id, document_id, seq, content, search, flags, embedding, embedding_model) VALUES ($1, $2, $3, $4, to_tsvector('simple', $4) || to_tsvector('simple', $5), $6, $7, $8)`,
				tenant, id, i, c, cmi.Bigrams(c), flags, vec, vecModel)
		}
		for i := range chunks { // D26
			b.Queue(`INSERT INTO projection_events (tenant_id, kind, object_id, seq, version, action) VALUES ($1, 'chunk', $2, $3, 1, 'upsert')`, tenant, id, i)
		}
		if err := tx.SendBatch(ctx, b).Close(); err != nil {
			return err
		}
		if err := insertEntities(ctx, tx, tenant, "knowledge", id, in.Entities); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "knowledge.create", id, map[string]any{"uri": in.URI, "version": version, "chunks": len(chunks), "sensitivity": in.Sensitivity, "flags": docFlags, "entities": len(in.Entities)}); err != nil {
			return err
		}
		out, err = scanDoc(tx.QueryRow(ctx, `SELECT `+docCols+` FROM knowledge_documents d JOIN sources s ON s.id = d.source_id WHERE d.id = $1`, id))
		return err
	})
	return out, err
}

// GetDocument returns a document's metadata (administrators).
func (s *Store) GetDocument(ctx context.Context, tenant, id string) (*cmi.Document, error) {
	var out *cmi.Document
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = scanDoc(tx.QueryRow(ctx, `SELECT `+docCols+` FROM knowledge_documents d JOIN sources s ON s.id = d.source_id WHERE d.id = $1`, id))
		return err
	})
	return out, err
}

// ListDocuments returns the tenant's documents, newest first.
func (s *Store) ListDocuments(ctx context.Context, tenant string, limit int) ([]cmi.Document, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := []cmi.Document{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+docCols+` FROM knowledge_documents d JOIN sources s ON s.id = d.source_id ORDER BY d.created_at DESC LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDoc(rows)
			if err != nil {
				return err
			}
			out = append(out, *d)
		}
		return rows.Err()
	})
	return out, err
}

// RevokeDocument withdraws a document (D6): it leaves retrieval, packages citing it read as stale.
func (s *Store) RevokeDocument(ctx context.Context, tenant, id, by, reason string, ifMatch int) (*cmi.Document, error) {
	var out *cmi.Document
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE knowledge_documents SET status = 'revoked', revoked_at = now(), revoked_by = $2, revoked_reason = $3, revision = revision + 1
			WHERE id = $1 AND status IN ('active', 'superseded') AND ($4 = 0 OR revision = $4)`, id, by, reason, ifMatch)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			if ifMatch != 0 {
				var current int
				if err := tx.QueryRow(ctx, `SELECT revision FROM knowledge_documents WHERE id = $1`, id).Scan(&current); err == nil && current != ifMatch {
					return fail("CONFLICT", "document %s is at revision %d, not %d", id, current, ifMatch)
				}
			}
			return fail("NOT_FOUND", "no revocable document %s", id)
		}
		if err := Audit(ctx, tx, tenant, by, "knowledge.revoke", id, map[string]any{"reason": reason}); err != nil {
			return err
		}
		out, err = scanDoc(tx.QueryRow(ctx, `SELECT `+docCols+` FROM knowledge_documents d JOIN sources s ON s.id = d.source_id WHERE d.id = $1`, id))
		return err
	})
	return out, err
}

// knowledgeHit is a retrieval result: a chunk with its document.
type knowledgeHit struct {
	Document cmi.Document
	Seq      int
	Content  string
	Trust    string
	Flags    []string
	Hit
}

// SearchKnowledge retrieves chunks of documents visible to v, lexically and semantically (D11),
// merged per chunk. Flagged chunks stay out unless the policy lets them through (D12).
func (s *Store) SearchKnowledge(ctx context.Context, tenant string, q Query, sources []string, v Visibility) ([]knowledgeHit, error) {
	if sources == nil {
		sources = []string{}
	}
	entities := q.Entities
	if entities == nil {
		entities = []string{}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	const from = ` FROM knowledge_chunks c JOIN knowledge_documents d ON d.id = c.document_id JOIN sources s ON s.id = d.source_id, LATERAL (SELECT d.status, d.valid_until, d.sensitivity, d.acl_subjects, d.acl_roles) x`
	filters := func(usable string) string {
		return ` AND (cardinality($5::text[]) = 0 OR s.name = ANY($5))` + usable + ` AND ($6 OR cardinality(c.flags) = 0)` + entityFilterSQL("knowledge", "d.id", "$7") + excludeSQL("$9")
	}
	lexPred, vecPred := filters(visibleSQL), filters(visibleSQL)
	var asOfArgs []any
	if q.AsOf != nil {
		lexPred, vecPred = filters(knowledgeAsOfSQL("$10", "$11")), filters(knowledgeAsOfSQL("$11", "$12"))
		asOfArgs = []any{q.AsOf.Valid, q.AsOf.RecordedArg()}
	}
	type key struct {
		id  string
		seq int
	}
	byKey := map[key]*knowledgeHit{}
	var order []key
	scan := func(rows pgx.Rows, semantic bool) error {
		defer rows.Close()
		for rows.Next() {
			var h knowledgeHit
			d := &h.Document
			var validUntil, revokedAt *time.Time
			var revokedBy, revokedReason, entities string
			var r float64
			if err := rows.Scan(&d.ID, &d.Source, &d.URI, &d.Title, &d.Version, &d.ContentSHA256, &d.ByteSize, &d.ChunkCount, &d.Status, &validUntil, &d.Sensitivity, &d.ACL.Subjects, &d.ACL.Roles,
				&revokedAt, &revokedBy, &revokedReason, &d.CreatedBy, &d.CreatedAt, &d.Flags, &d.Revision, &entities, &h.Seq, &h.Content, &h.Flags, &r, &h.Trust); err != nil {
				return err
			}
			if validUntil != nil {
				d.ValidUntil = *validUntil
			}
			d.Entities = decodeEntities(entities)
			if len(d.Flags) == 0 {
				d.Flags = nil
			}
			if len(h.Flags) == 0 {
				h.Flags = nil
			}
			hit := Hit{}
			if semantic {
				hit.Semantic = r
			} else {
				hit.Lexical = r
			}
			k := key{d.ID, h.Seq}
			if ex := byKey[k]; ex != nil {
				ex.merge(hit)
				continue
			}
			h.merge(hit)
			byKey[k] = &h
			order = append(order, k)
		}
		return rows.Err()
	}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if q.TSQuery != "" {
			rows, err := tx.Query(ctx, `SELECT `+docCols+`, c.seq, c.content, c.flags, ts_rank(c.search, q)::float8 AS r, s.trust`+from+`, to_tsquery('simple', $4) q
				WHERE c.search @@ q`+lexPred+` ORDER BY r DESC LIMIT $8`, append([]any{v.Subject, v.Roles, rank(v.Ceiling), q.TSQuery, sources, q.IncludeFlagged, entities, limit, excluded(v)}, asOfArgs...)...)
			if err != nil {
				return err
			}
			if err := scan(rows, false); err != nil {
				return err
			}
		}
		if len(q.Vector) > 0 && q.Model != "" {
			rows, err := tx.Query(ctx, `SELECT `+docCols+`, c.seq, c.content, c.flags, cosine_similarity(c.embedding, $4::real[])::float8 AS r, s.trust`+from+`
				WHERE c.embedding IS NOT NULL AND c.embedding_model = $10`+vecPred+` ORDER BY r DESC NULLS LAST LIMIT $8`, append([]any{v.Subject, v.Roles, rank(v.Ceiling), q.Vector, sources, q.IncludeFlagged, entities, limit, excluded(v), q.Model}, asOfArgs...)...)
			if err != nil {
				return err
			}
			if err := scan(rows, true); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]knowledgeHit, 0, len(order))
	for _, k := range order {
		if h := byKey[k]; h.Semantic > 0 || h.Lexical > 0 {
			out = append(out, *h)
		}
	}
	return out, nil
}
