package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"idp-cmi/internal/guard"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
)

// windowSQL says that the document a's validity window contains the instant at: a NULL valid_from means "from the
// beginning" (D52).
func windowSQL(a, at string) string {
	return `(` + a + `.valid_from IS NULL OR ` + a + `.valid_from <= ` + at + `) AND (` + a + `.valid_until IS NULL OR ` + a + `.valid_until > ` + at + `)`
}

// newerStartedSQL is true when some later version of document a's (base, uri) has started by instant at and has not
// been revoked or erased (D52). The later version's own end does not matter: when the newest version has expired there
// is no current version, and an older one does not come back. Revocation is the rollback: a revoked version is not counted.
func newerStartedSQL(a, at string) string {
	return `EXISTS (
		SELECT 1 FROM knowledge_documents d2
		WHERE d2.knowledge_base_id = ` + a + `.knowledge_base_id AND d2.uri = ` + a + `.uri AND d2.version > ` + a + `.version
		  AND d2.erasure_id IS NULL AND d2.status IN ('active', 'superseded', 'expired')
		  AND (d2.valid_from IS NULL OR d2.valid_from <= ` + at + `)
	)`
}

// notReplacedSQL keeps document a only when no later version of its (base, uri) has started at instant at.
func notReplacedSQL(a, at string) string {
	return `
	AND NOT ` + newerStartedSQL(a, at)
}

// knowledgeCurrentSQL is the one definition of "current" (D52) that retrieval, entity lists, omitted counts, quotas and
// package verification share: among the versions of a (base, uri) that are not revoked or erased and have started,
// take the highest; it is current when its window still contains now. If that version has ended there is no current
// version (no fallback to an older one). It adds that the base is active.
const knowledgeCurrentSQL = ` AND d.erasure_id IS NULL AND d.status IN ('active', 'superseded') AND (d.valid_until IS NULL OR d.valid_until > now())
	AND (d.valid_from IS NULL OR d.valid_from <= now())
	AND NOT ` + `EXISTS (
		SELECT 1 FROM knowledge_documents d2
		WHERE d2.knowledge_base_id = d.knowledge_base_id AND d2.uri = d.uri AND d2.version > d.version
		  AND d2.erasure_id IS NULL AND d2.status IN ('active', 'superseded', 'expired')
		  AND (d2.valid_from IS NULL OR d2.valid_from <= now())
	)
	AND EXISTS (SELECT 1 FROM knowledge_bases kbc WHERE kbc.id = d.knowledge_base_id AND kbc.status = 'active')`

// docStatusSQL is the status a document is shown with: what is stored (revoked, expired), and for a live version
// whether a newer one has started (superseded) or not (active).
const docStatusSQL = `CASE WHEN d.status IN ('active', 'superseded') THEN
		CASE WHEN ` + `EXISTS (
			SELECT 1 FROM knowledge_documents d2
			WHERE d2.knowledge_base_id = d.knowledge_base_id AND d2.uri = d.uri AND d2.version > d.version
			  AND d2.erasure_id IS NULL AND d2.status IN ('active', 'superseded', 'expired')
			  AND (d2.valid_from IS NULL OR d2.valid_from <= now())
		) THEN 'superseded' ELSE 'active' END
	ELSE d.status END`

const docCols = `d.id::text, s.name, d.uri, CASE WHEN d.erasure_id IS NULL THEN d.title ELSE '[erased]' END, d.version, d.content_sha256, d.byte_size, d.chunk_count, ` + docStatusSQL + `, d.valid_until, d.sensitivity, d.acl_subjects, d.acl_roles,
	d.revoked_at, coalesce(d.revoked_by, ''), coalesce(d.revoked_reason, ''), d.created_by, d.created_at, d.flags, d.revision, ` +
	"(SELECT coalesce(json_agg(json_build_object('type', e.entity_type, 'id', e.entity_id) ORDER BY e.entity_type, e.entity_id), '[]')::text FROM entity_refs e WHERE e.object_kind = 'knowledge' AND e.object_id = d.id), " +
	"coalesce((SELECT kb.name FROM knowledge_bases kb WHERE kb.id = d.knowledge_base_id), ''), d.valid_from, coalesce(d.doc_type, ''), CASE WHEN d.erasure_id IS NULL THEN d.owner ELSE '' END, d.review_due, " +
	"coalesce(d.original_media_type, ''), coalesce(d.original_byte_size, 0), coalesce(d.original_sha256, ''), " + ingestSQL

// ingestSQL is the latest conversion job of the document as JSON (D63): its state, attempts, the code of its failure and the
// notice for the uploader, never the detail, which may quote the converter.
const ingestSQL = `coalesce((SELECT json_build_object('state', j.state, 'attempts', j.attempts, 'error', coalesce(j.error_code, ''), 'notice', coalesce(j.notice, ''),
	'updatedAt', j.updated_at, 'nextAttemptAt', CASE WHEN j.state = 'queued' AND j.attempts > 0 THEN j.next_attempt_at END)::text
	FROM knowledge_conversions j WHERE j.document_id = d.id ORDER BY j.created_at DESC LIMIT 1), '')`

// docScan holds what scanning a document row needs besides the document itself.
type docScan struct {
	validUntil, revokedAt, validFrom, reviewDue *time.Time
	revokedBy, revokedReason, entities          string
	originalMedia, originalSum                  string
	originalSize                                int64
	ingest                                      string
}

// dest lists the scan targets of docCols for d.
func (x *docScan) dest(d *cmi.Document) []any {
	return []any{&d.ID, &d.Source, &d.URI, &d.Title, &d.Version, &d.ContentSHA256, &d.ByteSize, &d.ChunkCount, &d.Status, &x.validUntil, &d.Sensitivity, &d.ACL.Subjects, &d.ACL.Roles,
		&x.revokedAt, &x.revokedBy, &x.revokedReason, &d.CreatedBy, &d.CreatedAt, &d.Flags, &d.Revision, &x.entities, &d.KnowledgeBase, &x.validFrom, &d.DocType, &d.Owner, &x.reviewDue, &x.originalMedia, &x.originalSize, &x.originalSum, &x.ingest}
}

func (x *docScan) finish(d *cmi.Document) {
	d.Entities = decodeEntities(x.entities)
	if len(d.Flags) == 0 {
		d.Flags = nil
	}
	if x.validUntil != nil {
		d.ValidUntil = *x.validUntil
	}
	if x.validFrom != nil {
		d.ValidFrom = *x.validFrom
	}
	if x.reviewDue != nil {
		d.ReviewDue = *x.reviewDue
	}
	setOriginal(d, x.originalMedia, x.originalSize, x.originalSum)
	d.Ingest = decodeIngest(x.ingest)
	if x.revokedAt != nil {
		d.Revocation = &cmi.Revocation{At: *x.revokedAt, By: x.revokedBy, Reason: x.revokedReason}
	}
	if d.ACL.Subjects == nil {
		d.ACL.Subjects = []string{}
	}
	if d.ACL.Roles == nil {
		d.ACL.Roles = []string{}
	}
}

func scanDoc(row pgx.Row) (*cmi.Document, error) {
	var d cmi.Document
	var x docScan
	if err := row.Scan(x.dest(&d)...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "document not found")
		}
		return nil, err
	}
	x.finish(&d)
	return &d, nil
}

// CreateDocument indexes a document for an administrator or a connector (by is the writer's name). With
// no knowledgeBase named it goes into the enterprise base named after its source, with the uri it came with
// (connectors keep their uris; so does this legacy path). Naming a base, the uri must be kb://<base>/<path> (D51).
// A version's identity is (tenant, base, uri): a new version retires an older one of the same base only.
func (s *Store) CreateDocument(ctx context.Context, tenant string, in cmi.Document, text, by string) (*cmi.Document, error) {
	return s.createDocument(ctx, tenant, in, text, by, Visibility{Subject: by, Roles: []string{"cmi-admin"}}, in.KnowledgeBase != "")
}

// CreateMemberDocument indexes a document for a caller who is not an administrator.
// With no knowledge base named, the document goes into the caller's personal base, created on first use.
// The uri is kb://<base>/<path> of the base it lands in.
func (s *Store) CreateMemberDocument(ctx context.Context, tenant string, in cmi.Document, text string, v Visibility) (*cmi.Document, error) {
	return s.createDocument(ctx, tenant, in, text, v.Subject, v, true)
}

func (s *Store) createDocument(ctx context.Context, tenant string, in cmi.Document, text, by string, v Visibility, strictURI bool) (*cmi.Document, error) {
	defaultSensitivity := in.Sensitivity == ""
	if defaultSensitivity {
		in.Sensitivity = cmi.SensitivityInternal // replaced by the base's default once the base is known
	}
	if err := cmi.ValidateDocument(&in, text); err != nil {
		return nil, fail("INVALID", "%s", err)
	}
	if in.ACL.Subjects == nil {
		in.ACL.Subjects = []string{}
	}
	if in.ACL.Roles == nil {
		in.ACL.Roles = []string{}
	}
	if base := cmi.BaseOfURI(in.URI); base != "" && in.KnowledgeBase != "" && base != in.KnowledgeBase {
		return nil, fail("INVALID", "uri %s names knowledge base %s, not %s", in.URI, base, in.KnowledgeBase)
	}
	parts := cmi.ChunkKnowledge(text)
	if len(parts) == 0 {
		return nil, fail("INVALID", "text produced no chunks")
	}
	bodies := make([]string, len(parts))
	indexes := make([]string, len(parts))
	for i, part := range parts {
		bodies[i] = part.Body
		indexes[i] = part.IndexText(in.Title, s.Knowledge.IndexContext)
	}
	chunkFlags := make([][]string, len(parts))
	var docFlags []string
	for i := range bodies {
		chunkFlags[i] = guard.Scan(parts[i].Scanned()) // D12: the text a model will see - the heading path line and the body - and what is indexed
		docFlags = guard.Union(docFlags, chunkFlags[i])
	}
	if docFlags == nil {
		docFlags = []string{}
	}
	in.Entities = cmi.SortEntities(in.Entities)
	var vecs [][]float32
	var model string
	if !defaultSensitivity { // D29: content above the tenant's external-model ceiling is never embedded; a default sensitivity is known only inside the transaction, and the backfill covers it
		if pol, err := s.GetPolicy(ctx, tenant); err == nil && cmi.Within(in.Sensitivity, pol.ExternalModelCeiling) {
			vecs, model = s.embed(ctx, indexes)
		}
	}
	var out *cmi.Document
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		src, err := getSource(ctx, tx, in.Source)
		if err != nil {
			return fail("CONFLICT", "source %s is not registered", in.Source)
		}
		b, err := s.resolveBase(ctx, tx, tenant, src, in.KnowledgeBase, by, v)
		if err != nil {
			return err
		}
		if b.Status != cmi.KnowledgeBaseActive {
			return fail("CONFLICT", "knowledge base %s is archived", b.Name)
		}
		if b.SourceID != src.ID {
			return fail("INVALID", "document source %s is not the knowledge base's source", src.Name)
		}
		if defaultSensitivity {
			in.Sensitivity = b.DefaultSensitivity
		}
		if !isAdmin(v.Roles) && v.Ceiling != "" && !cmi.Within(in.Sensitivity, v.Ceiling) {
			return fail("FORBIDDEN", "document sensitivity %s is above the caller's ceiling", in.Sensitivity)
		}
		if !cmi.Within(in.Sensitivity, b.Ceiling) {
			return fail("INVALID", "document sensitivity %s is above the knowledge base ceiling %s", in.Sensitivity, b.Ceiling)
		}
		if in.KnowledgeBase == "" && b.ScopeKind == cmi.ScopeSubject && strings.HasPrefix(in.URI, cmi.PersonalAliasPrefix) {
			in.URI = cmi.KnowledgeURIPrefix(b.Name) + strings.TrimPrefix(in.URI, cmi.PersonalAliasPrefix) // D53: stored under the canonical name
		}
		prefix := cmi.KnowledgeURIPrefix(b.Name)
		if strictURI && !strings.HasPrefix(in.URI, prefix) {
			return fail("INVALID", "an upload into knowledge base %s uses a uri of the form %s<path>", b.Name, prefix)
		}
		if other := cmi.BaseOfURI(in.URI); other != "" && other != b.Name {
			return fail("INVALID", "uri %s names knowledge base %s, not %s", in.URI, other, b.Name)
		}
		inForce := in.ValidFrom.IsZero() || !in.ValidFrom.After(s.now())
		if err := s.checkNewVersionQuota(ctx, tx, b, in.URI, len(text), inForce); err != nil {
			return err
		}
		var version int
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) + 1 FROM knowledge_documents WHERE knowledge_base_id = $1 AND uri = $2`, b.ID, in.URI).Scan(&version); err != nil {
			return err
		}
		var validFrom, validUntil, reviewDue any
		if !in.ValidFrom.IsZero() {
			validFrom = in.ValidFrom
		}
		if !in.ValidUntil.IsZero() {
			validUntil = in.ValidUntil
		}
		if !in.ReviewDue.IsZero() {
			reviewDue = in.ReviewDue
		}
		id := NewUUID()
		if _, err := tx.Exec(ctx, `INSERT INTO knowledge_documents (id, tenant_id, source_id, knowledge_base_id, uri, title, version, content_sha256, byte_size, chunk_count, valid_from, valid_until, doc_type, owner, review_due, sensitivity, acl_subjects, acl_roles, created_by, flags)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
			id, tenant, src.ID, b.ID, in.URI, in.Title, version, cmi.ContentSHA256(text), len(text), len(parts), validFrom, validUntil, in.DocType, in.Owner, reviewDue, in.Sensitivity, in.ACL.Subjects, in.ACL.Roles, by, docFlags); err != nil {
			return err
		}
		batch := &pgx.Batch{}
		for i, part := range parts {
			var vec, vecModel any
			if vecs != nil {
				vec, vecModel = vecs[i], model
			}
			flags := chunkFlags[i]
			if flags == nil {
				flags = []string{}
			}
			queueChunk(batch, tenant, id, i, part, indexes[i], flags, vec, vecModel)
		}
		for i := range parts { // D26
			batch.Queue(`INSERT INTO projection_events (tenant_id, kind, object_id, seq, version, action) VALUES ($1, 'chunk', $2, $3, 1, 'upsert')`, tenant, id, i)
		}
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return err
		}
		if err := insertEntities(ctx, tx, tenant, "knowledge", id, in.Entities); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "knowledge.create", id, map[string]any{"uri": in.URI, "version": version, "base": b.ID, "chunks": len(parts), "sensitivity": in.Sensitivity, "flags": docFlags, "entities": len(in.Entities)}); err != nil {
			return err
		}
		out, err = scanDoc(tx.QueryRow(ctx, `SELECT `+docCols+` FROM knowledge_documents d JOIN sources s ON s.id = d.source_id WHERE d.id = $1`, id))
		return err
	})
	return out, err
}

// resolveBase finds the base an upload goes into and the caller's right to write it (S4, D53). The returned row is
// locked until the end of the transaction, which serializes the version number and the quota check of the base.
func (s *Store) resolveBase(ctx context.Context, tx pgx.Tx, tenant string, src *cmi.Source, name, by string, v Visibility) (*baseRow, error) {
	var b *baseRow
	var err error
	switch {
	case name != "":
		b, err = loadBase(ctx, tx, name, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "knowledge base %s is not registered", name)
		}
	case !isAdmin(v.Roles):
		b, err = ensurePersonalBase(ctx, tx, tenant, src, v.Subject)
	default:
		b, err = ensureSourceBase(ctx, tx, tenant, src, by)
	}
	if err != nil {
		return nil, err
	}
	if err := b.canWrite(ctx, tx, v, true); err != nil {
		return nil, err
	}
	return b, nil
}

// GetDocument returns a document's metadata (administrators). A document in another member's personal base is read
// by naming its id, and the read is audited (D53).
func (s *Store) GetDocument(ctx context.Context, tenant, id string, v Visibility) (*cmi.Document, error) {
	var out *cmi.Document
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = scanDoc(tx.QueryRow(ctx, `SELECT `+docCols+` FROM knowledge_documents d JOIN sources s ON s.id = d.source_id WHERE d.id = $1`, id))
		if err != nil {
			return err
		}
		return auditPersonalRead(ctx, tx, tenant, v, out.KnowledgeBase, out.ID, "document")
	})
	return out, err
}

// ListDocuments returns the tenant's documents, newest first. The documents of members' personal bases are not in the
// list unless the base is named; naming another member's base is audited (D53).
func (s *Store) ListDocuments(ctx context.Context, tenant string, v Visibility, base string, limit int) ([]cmi.Document, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := []cmi.Document{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if base != "" {
			if err := auditPersonalBase(ctx, tx, tenant, v, base, "list"); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `SELECT `+docCols+` FROM knowledge_documents d JOIN sources s ON s.id = d.source_id
			JOIN knowledge_bases kb ON kb.id = d.knowledge_base_id
			WHERE ($2 = '' AND (kb.scope_kind <> 'subject' OR kb.scope_id = $1)) OR ($2 <> '' AND kb.name = $2)
			ORDER BY d.created_at DESC LIMIT $3`, v.Subject, base, limit)
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

// auditNamedHits records, for each other member whose personal base supplied chunks to a resolve, that the
// caller read it (D53): knowledge.read_other, like D42's memory.read_other.
func auditNamedHits(ctx context.Context, tx pgx.Tx, tenant string, v Visibility, counts map[string]int) error {
	if len(v.NamedSubjects) == 0 || len(counts) == 0 {
		return nil
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	rows, err := tx.Query(ctx, `SELECT id::text, name, scope_id FROM knowledge_bases WHERE name = ANY($1::text[]) AND scope_kind = 'subject' AND scope_id <> $2 ORDER BY name`, names, v.Subject)
	if err != nil {
		return err
	}
	type named struct{ id, name, subject string }
	var hits []named
	for rows.Next() {
		var n named
		if err := rows.Scan(&n.id, &n.name, &n.subject); err != nil {
			rows.Close()
			return err
		}
		hits = append(hits, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, n := range hits {
		if err := Audit(ctx, tx, tenant, v.Subject, "knowledge.read_other", n.id, map[string]any{"via": "resolve", "subject": n.subject, "chunks": counts[n.name], "client": v.Client, "actor": v.Actor}); err != nil {
			return err
		}
	}
	return nil
}

// auditPersonalBase audits naming another member's personal base (a list of its documents).
func auditPersonalBase(ctx context.Context, tx pgx.Tx, tenant string, v Visibility, base, via string) error {
	return auditPersonalRead(ctx, tx, tenant, v, base, "", via)
}

// revoke withdraws a document: it leaves retrieval, packages citing it read as stale (D6). A stale If-Match
// is a conflict, on every path (D19).
func revokeDocument(ctx context.Context, tx pgx.Tx, id, by, reason string, ifMatch int) error {
	tag, err := tx.Exec(ctx, `UPDATE knowledge_documents SET status = 'revoked', revoked_at = now(), revoked_by = $2, revoked_reason = $3, revision = revision + 1
		WHERE id = $1 AND status IN ('active', 'superseded', 'pending') AND ($4 = 0 OR revision = $4)`, id, by, reason, ifMatch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if ifMatch != 0 {
			var current int
			var status string
			if err := tx.QueryRow(ctx, `SELECT revision, status FROM knowledge_documents WHERE id = $1`, id).Scan(&current, &status); err == nil && current != ifMatch && (status == "active" || status == "superseded") {
				return fail("CONFLICT", "document %s is at revision %d, not %d", id, current, ifMatch)
			}
		}
		return fail("NOT_FOUND", "no revocable document %s", id)
	}
	return nil
}

// RevokeDocument withdraws a document (administrators).
func (s *Store) RevokeDocument(ctx context.Context, tenant, id, by, reason string, ifMatch int) (*cmi.Document, error) {
	var out *cmi.Document
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := revokeDocument(ctx, tx, id, by, reason, ifMatch); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "knowledge.revoke", id, map[string]any{"reason": reason}); err != nil {
			return err
		}
		var err error
		out, err = scanDoc(tx.QueryRow(ctx, `SELECT `+docCols+` FROM knowledge_documents d JOIN sources s ON s.id = d.source_id WHERE d.id = $1`, id))
		return err
	})
	return out, err
}

// memberDocSQL is what a member route may read of a document: not erased, within the caller's sensitivity ceiling
// and ACL (D7), not from an excluded source (D23), in a base of the caller's default scope (D53). An administrator
// naming a document by its id reads it from any base (a personal one audited), so the base scope is left to the caller.
// Placeholders: $1 subject, $2 roles, $3 ceiling rank, $4 excluded sources.
func memberDocSQL(v Visibility) string {
	scope := kbScopeSQL
	if isAdmin(v.Roles) {
		scope = ""
	}
	return ` AND d.erasure_id IS NULL` + aclSQL + excludeSQL("$4") + scope
}

// GetMemberDocument returns a document the caller can read, including one that is not yet in force.
func (s *Store) GetMemberDocument(ctx context.Context, tenant, id string, v Visibility) (*cmi.Document, error) {
	var out *cmi.Document
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		if err := bindView(ctx, tx, v); err != nil {
			return err
		}
		out, err = scanDoc(tx.QueryRow(ctx, `SELECT `+docCols+` FROM knowledge_documents d JOIN sources s ON s.id = d.source_id, LATERAL (SELECT d.status, d.valid_until, d.sensitivity, d.acl_subjects, d.acl_roles) x
			WHERE d.id = $5`+memberDocSQL(v), v.Subject, rolesOrEmpty(v.Roles), rank(v.Ceiling), excluded(v), id))
		if err != nil {
			return err
		}
		return auditPersonalRead(ctx, tx, tenant, v, out.KnowledgeBase, out.ID, "document")
	})
	return out, err
}

// ListMemberDocuments returns the documents in bases the caller may write, newest first, that the caller can also
// read: the same predicate as GetMemberDocument (D53).
func (s *Store) ListMemberDocuments(ctx context.Context, tenant string, v Visibility, limit int) ([]cmi.Document, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := []cmi.Document{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := bindView(ctx, tx, Visibility{Subject: v.Subject, Roles: v.Roles}); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+docCols+` FROM knowledge_documents d
			JOIN sources s ON s.id = d.source_id
			JOIN knowledge_bases kb ON kb.id = d.knowledge_base_id, LATERAL (SELECT d.status, d.valid_until, d.sensitivity, d.acl_subjects, d.acl_roles) x
			WHERE ((kb.scope_kind = 'subject' AND kb.scope_id = $1) OR $1 = ANY(kb.manager_subjects) OR kb.manager_roles && $2::text[])
			  AND d.erasure_id IS NULL`+aclSQL+excludeSQL("$4")+kbScopeSQL+`
			ORDER BY d.created_at DESC LIMIT $5`, v.Subject, rolesOrEmpty(v.Roles), rank(v.Ceiling), excluded(v), limit)
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

// RevokeMemberDocument withdraws a document in a base the caller may write and can read (D53). A document the caller
// cannot read, or in a base the caller cannot write, is not found. A stale If-Match is a conflict, as on the
// administrator's route.
func (s *Store) RevokeMemberDocument(ctx context.Context, tenant, id, reason string, v Visibility, ifMatch int) (*cmi.Document, error) {
	var out *cmi.Document
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := bindView(ctx, tx, v); err != nil {
			return err
		}
		var name string
		err := tx.QueryRow(ctx, `SELECT kb.name FROM knowledge_documents d JOIN sources s ON s.id = d.source_id JOIN knowledge_bases kb ON kb.id = d.knowledge_base_id, LATERAL (SELECT d.status, d.valid_until, d.sensitivity, d.acl_subjects, d.acl_roles) x
			WHERE d.id = $5`+memberDocSQL(v), v.Subject, rolesOrEmpty(v.Roles), rank(v.Ceiling), excluded(v), id).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail("NOT_FOUND", "no revocable document %s", id)
		}
		if err != nil {
			return err
		}
		b, err := loadBase(ctx, tx, name, false)
		if err != nil {
			return err
		}
		if err := b.canWrite(ctx, tx, v, false); err != nil {
			if se, ok := err.(*Error); ok && (se.Code == "FORBIDDEN" || se.Code == "NOT_FOUND") {
				return fail("NOT_FOUND", "no revocable document %s", id)
			}
			return err
		}
		if err := revokeDocument(ctx, tx, id, v.Subject, reason, ifMatch); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, v.Subject, "knowledge.revoke", id, map[string]any{"reason": reason}); err != nil {
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
	Locator  *cmi.Locator
	Hit
}

// SearchKnowledge retrieves chunks of documents visible to v, lexically and semantically (D11),
// merged per chunk. Flagged chunks stay out unless the policy lets them through (D12). The documents are the
// current ones (D52) in the caller's default scope plus the personal bases the request named (D53).
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
	const locator = `, c.heading_path, c.page, c.table_name, c.slide, c.sheet, c.cell_range`
	// the placeholders after $9 differ by retriever: the lexical statement has the base and type filters at $10 and $11, the
	// vector one has the model at $10 and the filters at $11 and $12 (D72); a bitemporal view follows them.
	filters := func(usable, kb, dt string) string {
		return ` AND (cardinality($5::text[]) = 0 OR s.name = ANY($5))` + usable + kbScopeSQL + ` AND ($6 OR cardinality(c.flags) = 0)` + entityFilterSQL("knowledge", "d.id", "$7") + excludeSQL("$9") + baseDocTypeSQL(kb, dt)
	}
	lexPred, vecPred := filters(aclSQL+knowledgeCurrentSQL, "$10", "$11"), filters(aclSQL+knowledgeCurrentSQL, "$11", "$12")
	var asOfArgs []any
	if q.AsOf != nil {
		lexPred, vecPred = filters(knowledgeAsOfSQL("$12", "$13"), "$10", "$11"), filters(knowledgeAsOfSQL("$13", "$14"), "$11", "$12")
		asOfArgs = []any{q.AsOf.Valid, q.AsOf.RecordedArg()}
	}
	filterArgs := []any{orEmpty(q.KnowledgeBases), orEmpty(q.DocTypes)}
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
			var x docScan
			var r float64
			var path, table, sheet, cells string
			var page, slide *int
			dest := append(x.dest(d), &h.Seq, &h.Content, &h.Flags, &r, &h.Trust, &path, &page, &table, &slide, &sheet, &cells)
			if err := rows.Scan(dest...); err != nil {
				return err
			}
			x.finish(d)
			if len(h.Flags) == 0 {
				h.Flags = nil
			}
			if path != "" || page != nil || table != "" || slide != nil || sheet != "" {
				h.Locator = &cmi.Locator{Table: table, Sheet: sheet, Range: cells}
				if slide != nil {
					h.Locator.Slide = *slide
				}
				if path != "" {
					h.Locator.Headings = cmi.SplitHeadingPath(path)
				}
				if page != nil {
					h.Locator.Page = *page
				}
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
		if err := bindView(ctx, tx, v); err != nil {
			return err
		}
		if q.TSQuery != "" {
			// Equal scores are common (a CJK query matches by bigrams): they are ordered by uri, version and chunk, so the
			// order does not depend on the plan the database chose (ADR-0001 D56).
			rows, err := tx.Query(ctx, `SELECT `+docCols+`, c.seq, c.content, c.flags, ts_rank(c.search, q)::float8 AS r, s.trust`+locator+from+`, to_tsquery('simple', $4) q
				WHERE c.search @@ q`+lexPred+` ORDER BY r DESC, d.uri, d.version, c.seq LIMIT $8`, append(append([]any{v.Subject, rolesOrEmpty(v.Roles), rank(v.Ceiling), q.TSQuery, sources, q.IncludeFlagged, entities, limit, excluded(v)}, filterArgs...), asOfArgs...)...)
			if err != nil {
				return err
			}
			if err := scan(rows, false); err != nil {
				return err
			}
		}
		if len(q.Vector) > 0 && q.Model != "" {
			score, vfilter, order, ef := s.semanticSQL(ctx, "knowledge_chunks", "c", len(q.Vector))
			if ef > 0 {
				if err := tuneHNSW(ctx, tx, limit); err != nil {
					return err
				}
			}
			rows, err := tx.Query(ctx, `SELECT `+docCols+`, c.seq, c.content, c.flags, `+score+` AS r, s.trust`+locator+from+`
				WHERE c.embedding IS NOT NULL AND c.embedding_model = $10`+vfilter+vecPred+` ORDER BY `+order+` LIMIT $8`, append(append([]any{v.Subject, rolesOrEmpty(v.Roles), rank(v.Ceiling), q.Vector, sources, q.IncludeFlagged, entities, limit, excluded(v), q.Model}, filterArgs...), asOfArgs...)...)
			if err != nil {
				return err
			}
			if err := scan(rows, true); err != nil {
				return err
			}
		}
		if err := auditNamedBases(ctx, tx, tenant, v, q.KnowledgeBases); err != nil {
			return err
		}
		counts := map[string]int{}
		for _, h := range byKey {
			if h.Semantic > 0 || h.Lexical > 0 {
				counts[h.Document.KnowledgeBase]++
			}
		}
		return auditNamedHits(ctx, tx, tenant, v, counts)
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

// queueChunk queues the insert of one chunk of a document with its locator (D49).
func queueChunk(batch *pgx.Batch, tenant, docID string, seq int, part cmi.LocatedChunk, index string, flags []string, vec any, model any) {
	var page, slide any
	if part.Page > 0 {
		page = part.Page
	}
	if part.Slide > 0 {
		slide = part.Slide
	}
	batch.Queue(`INSERT INTO knowledge_chunks (tenant_id, document_id, seq, content, index_text, heading_path, page, table_name, slide, sheet, cell_range, search, flags, embedding, embedding_model)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, to_tsvector('simple', $5) || to_tsvector('simple', $12), $13, $14, $15)`,
		tenant, docID, seq, part.Body, index, part.Path(), page, part.Table, slide, part.Sheet, part.Range, cmi.Bigrams(index), flags, vec, model)
}

// decodeIngest reads the JSON of ingestSQL.
func decodeIngest(raw string) *cmi.Ingest {
	if raw == "" {
		return nil
	}
	var j struct {
		State         string     `json:"state"`
		Attempts      int        `json:"attempts"`
		Error         string     `json:"error"`
		Notice        string     `json:"notice"`
		UpdatedAt     time.Time  `json:"updatedAt"`
		NextAttemptAt *time.Time `json:"nextAttemptAt"`
	}
	if json.Unmarshal([]byte(raw), &j) != nil || j.State == "" {
		return nil
	}
	out := &cmi.Ingest{State: j.State, Attempts: j.Attempts, Error: j.Error, Notice: j.Notice, UpdatedAt: j.UpdatedAt}
	if j.NextAttemptAt != nil {
		out.NextAttemptAt = *j.NextAttemptAt
	}
	return out
}
