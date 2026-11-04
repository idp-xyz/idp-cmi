package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
)

const docCols = `d.id::text, s.name, d.uri, d.title, d.version, d.content_sha256, d.byte_size, d.chunk_count, d.status, d.valid_until, d.sensitivity, d.acl_subjects, d.acl_roles,
	d.revoked_at, coalesce(d.revoked_by, ''), coalesce(d.revoked_reason, ''), d.created_by, d.created_at`

func scanDoc(row pgx.Row) (*cmi.Document, error) {
	var d cmi.Document
	var validUntil, revokedAt *time.Time
	var revokedBy, revokedReason string
	if err := row.Scan(&d.ID, &d.Source, &d.URI, &d.Title, &d.Version, &d.ContentSHA256, &d.ByteSize, &d.ChunkCount, &d.Status, &validUntil, &d.Sensitivity, &d.ACL.Subjects, &d.ACL.Roles,
		&revokedAt, &revokedBy, &revokedReason, &d.CreatedBy, &d.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "document not found")
		}
		return nil, err
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
		if _, err := tx.Exec(ctx, `UPDATE knowledge_documents SET status = 'superseded' WHERE uri = $1 AND status = 'active'`, in.URI); err != nil {
			return err
		}
		var validUntil any
		if !in.ValidUntil.IsZero() {
			validUntil = in.ValidUntil
		}
		id := NewUUID()
		if _, err := tx.Exec(ctx, `INSERT INTO knowledge_documents (id, tenant_id, source_id, uri, title, version, content_sha256, byte_size, chunk_count, valid_until, sensitivity, acl_subjects, acl_roles, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			id, tenant, src.ID, in.URI, in.Title, version, cmi.ContentSHA256(text), len(text), len(chunks), validUntil, in.Sensitivity, in.ACL.Subjects, in.ACL.Roles, by); err != nil {
			return err
		}
		b := &pgx.Batch{}
		for i, c := range chunks {
			b.Queue(`INSERT INTO knowledge_chunks (tenant_id, document_id, seq, content, search) VALUES ($1, $2, $3, $4, to_tsvector('simple', $4) || to_tsvector('simple', $5))`, tenant, id, i, c, cmi.Bigrams(c))
		}
		if err := tx.SendBatch(ctx, b).Close(); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "knowledge.create", id, map[string]any{"uri": in.URI, "version": version, "chunks": len(chunks), "sensitivity": in.Sensitivity}); err != nil {
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
func (s *Store) RevokeDocument(ctx context.Context, tenant, id, by, reason string) (*cmi.Document, error) {
	var out *cmi.Document
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE knowledge_documents SET status = 'revoked', revoked_at = now(), revoked_by = $2, revoked_reason = $3 WHERE id = $1 AND status IN ('active', 'superseded')`, id, by, reason)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
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
	Rank     float64
	Trust    string
}

// SearchKnowledge runs the full-text query over the chunks of documents visible to v.
func (s *Store) SearchKnowledge(ctx context.Context, tenant, tsquery string, sources []string, v Visibility, limit int) ([]knowledgeHit, error) {
	var out []knowledgeHit
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+docCols+`, c.seq, c.content, ts_rank(c.search, q) AS r, s.trust
			FROM knowledge_chunks c JOIN knowledge_documents d ON d.id = c.document_id JOIN sources s ON s.id = d.source_id, to_tsquery('simple', $4) q,
			LATERAL (SELECT d.status, d.valid_until, d.sensitivity, d.acl_subjects, d.acl_roles) x
			WHERE c.search @@ q AND (cardinality($5::text[]) = 0 OR s.name = ANY($5))`+visibleSQL+` ORDER BY r DESC LIMIT $6`,
			v.Subject, v.Roles, rank(v.Ceiling), tsquery, sources, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h knowledgeHit
			d := &h.Document
			var validUntil, revokedAt *time.Time
			var revokedBy, revokedReason string
			if err := rows.Scan(&d.ID, &d.Source, &d.URI, &d.Title, &d.Version, &d.ContentSHA256, &d.ByteSize, &d.ChunkCount, &d.Status, &validUntil, &d.Sensitivity, &d.ACL.Subjects, &d.ACL.Roles,
				&revokedAt, &revokedBy, &revokedReason, &d.CreatedBy, &d.CreatedAt, &h.Seq, &h.Content, &h.Rank, &h.Trust); err != nil {
				return err
			}
			if validUntil != nil {
				d.ValidUntil = *validUntil
			}
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}
