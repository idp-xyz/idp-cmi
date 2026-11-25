package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/adapters/blob"
	"idp-cmi/internal/canonical"
	"idp-cmi/internal/cmi"
)

// Erased is the tombstone text that replaces purged plaintext.
const Erased = "[erased]"

// ErasureSelector says what to erase: any combination; the barrier catches every record and
// document that matches any part.
type ErasureSelector struct {
	RecordIDs   []string    `json:"recordIds,omitempty"`
	DocumentIDs []string    `json:"documentIds,omitempty"`
	Ref         string      `json:"ref,omitempty"`     // provenance reference of records
	Subject     string      `json:"subject,omitempty"` // records in that subject's scope, the documents of their personal knowledge base, and the owner fields that name them (D57)
	Entity      *cmi.Entity `json:"entity,omitempty"`  // records and documents referencing it
	Reason      string      `json:"reason"`
}

// Validate checks a selector.
func (s *ErasureSelector) Validate() error {
	if len(s.RecordIDs)+len(s.DocumentIDs) == 0 && s.Ref == "" && s.Subject == "" && s.Entity == nil {
		return errors.New("select something: recordIds, documentIds, ref, subject or entity")
	}
	if len(s.RecordIDs) > 1000 || len(s.DocumentIDs) > 1000 {
		return errors.New("at most 1000 ids per erasure")
	}
	for _, id := range append(append([]string{}, s.RecordIDs...), s.DocumentIDs...) {
		if !IsUUID(id) {
			return fmt.Errorf("%q is not an id", id)
		}
	}
	if s.Entity != nil {
		if err := cmi.ValidateEntities([]cmi.Entity{*s.Entity}); err != nil {
			return err
		}
	}
	if strings.TrimSpace(s.Reason) == "" || len(s.Reason) > 500 {
		return errors.New("reason is required (at most 500 characters)")
	}
	return nil
}

// Domain is one purge domain's result.
type Domain struct {
	Status string    `json:"status"` // pending | done | failed
	Rows   int       `json:"rows"`
	At     time.Time `json:"at,omitzero"`
	Error  string    `json:"error,omitempty"`
}

// Domains the purge covers, in order.
var Domains = []string{"content", "vectors", "index", "entities", "packages", "audit", "originals"}

// Erasure is a request with its progress.
type Erasure struct {
	ID          string            `json:"erasureId"`
	RequestedBy string            `json:"requestedBy"`
	Reason      string            `json:"reason"`
	Selector    ErasureSelector   `json:"selector"`
	RecordIDs   []string          `json:"recordIds"`
	DocumentIDs []string          `json:"documentIds"`
	Status      string            `json:"status"` // pending | running | verified | failed
	Domains     map[string]Domain `json:"domains"`
	Attempts    int               `json:"attempts"`
	CreatedAt   time.Time         `json:"createdAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
}

const erasureCols = `id::text, requested_by, reason, selector, record_ids::text[], document_ids::text[], status, domains, attempts, created_at, updated_at`

func scanErasure(row pgx.Row) (*Erasure, error) {
	var e Erasure
	var sel, dom []byte
	if err := row.Scan(&e.ID, &e.RequestedBy, &e.Reason, &sel, &e.RecordIDs, &e.DocumentIDs, &e.Status, &dom, &e.Attempts, &e.CreatedAt, &e.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "erasure not found")
		}
		return nil, err
	}
	_ = json.Unmarshal(sel, &e.Selector)
	e.Domains = map[string]Domain{}
	_ = json.Unmarshal(dom, &e.Domains)
	for _, d := range Domains {
		if _, ok := e.Domains[d]; !ok {
			e.Domains[d] = Domain{Status: "pending"}
		}
	}
	return &e, nil
}

// RequestErasure records the request and sets the barrier in one transaction: every matching record
// and document gets erasure_id (and is revoked if still usable), so no read path serves its
// plaintext from now on. The purge itself is the Worker's (Purge).
func (s *Store) RequestErasure(ctx context.Context, tenant string, sel ErasureSelector, by string) (*Erasure, error) {
	if err := sel.Validate(); err != nil {
		return nil, fail("INVALID", "%s", err)
	}
	var out *Erasure
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		id := NewUUID()
		var recordIDs, documentIDs []string
		add := func(dst *[]string, q string, args ...any) error {
			rows, err := tx.Query(ctx, q, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var x string
				if err := rows.Scan(&x); err != nil {
					return err
				}
				*dst = append(*dst, x)
			}
			return rows.Err()
		}
		if len(sel.RecordIDs) > 0 {
			if err := add(&recordIDs, `SELECT id::text FROM memory_records WHERE id = ANY($1::uuid[]) AND erasure_id IS NULL`, sel.RecordIDs); err != nil {
				return err
			}
		}
		if len(sel.DocumentIDs) > 0 {
			if err := add(&documentIDs, `SELECT id::text FROM knowledge_documents WHERE id = ANY($1::uuid[]) AND erasure_id IS NULL`, sel.DocumentIDs); err != nil {
				return err
			}
		}
		if sel.Ref != "" {
			if err := add(&recordIDs, `SELECT id::text FROM memory_records WHERE prov_ref = $1 AND erasure_id IS NULL`, sel.Ref); err != nil {
				return err
			}
		}
		if sel.Subject != "" {
			if err := add(&recordIDs, `SELECT id::text FROM memory_records WHERE scope_kind = 'subject' AND scope_id = $1 AND erasure_id IS NULL`, sel.Subject); err != nil {
				return err
			}
		}
		if sel.Subject != "" {
			if err := add(&documentIDs, `SELECT d.id::text FROM knowledge_documents d JOIN knowledge_bases kb ON kb.id = d.knowledge_base_id
				WHERE kb.scope_kind = 'subject' AND kb.scope_id = $1 AND d.erasure_id IS NULL`, sel.Subject); err != nil {
				return err
			}
		}
		if sel.Entity != nil {
			if err := add(&recordIDs, `SELECT object_id::text FROM entity_refs WHERE object_kind = 'memory' AND entity_type = $1 AND entity_id = $2`, sel.Entity.Type, sel.Entity.ID); err != nil {
				return err
			}
			if err := add(&documentIDs, `SELECT object_id::text FROM entity_refs WHERE object_kind = 'knowledge' AND entity_type = $1 AND entity_id = $2`, sel.Entity.Type, sel.Entity.ID); err != nil {
				return err
			}
		}
		recordIDs, documentIDs = unique(recordIDs), unique(documentIDs)
		if len(recordIDs)+len(documentIDs) == 0 {
			return fail("NOT_FOUND", "nothing matches the selector (or it is already under erasure)")
		}
		// the barrier: from here no read path returns the plaintext
		if len(recordIDs) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE memory_records SET erasure_id = $1, version = version + 1,
				status = CASE WHEN status = 'revoked' THEN status ELSE 'revoked' END, revoked_at = coalesce(revoked_at, now()), revoked_by = coalesce(revoked_by, $2), revoked_reason = coalesce(revoked_reason, 'erasure')
				WHERE id = ANY($3::uuid[]) AND erasure_id IS NULL`, id, by, recordIDs); err != nil {
				return err
			}
		}
		if len(documentIDs) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE knowledge_documents SET erasure_id = $1, revision = revision + 1,
				status = CASE WHEN status = 'revoked' THEN status ELSE 'revoked' END, revoked_at = coalesce(revoked_at, now()), revoked_by = coalesce(revoked_by, $2), revoked_reason = coalesce(revoked_reason, 'erasure')
				WHERE id = ANY($3::uuid[]) AND erasure_id IS NULL`, id, by, documentIDs); err != nil {
				return err
			}
		}
		selRaw, _ := json.Marshal(sel)
		if _, err := tx.Exec(ctx, `INSERT INTO erasures (id, tenant_id, requested_by, reason, selector, record_ids, document_ids) VALUES ($1, $2, $3, $4, $5, $6::uuid[], $7::uuid[])`,
			id, tenant, by, sel.Reason, selRaw, recordIDs, documentIDs); err != nil {
			return err
		}
		// the audit of the request names ids and counts, never content
		if err := Audit(ctx, tx, tenant, by, "erasure.request", id, map[string]any{"records": len(recordIDs), "documents": len(documentIDs), "reason": sel.Reason}); err != nil {
			return err
		}
		var err error
		out, err = scanErasure(tx.QueryRow(ctx, `SELECT `+erasureCols+` FROM erasures WHERE id = $1`, id))
		return err
	})
	return out, err
}

func unique(xs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// GetErasure returns one erasure with its progress.
func (s *Store) GetErasure(ctx context.Context, tenant, id string) (*Erasure, error) {
	var out *Erasure
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = scanErasure(tx.QueryRow(ctx, `SELECT `+erasureCols+` FROM erasures WHERE id = $1`, id))
		return err
	})
	return out, err
}

// ListErasures returns a tenant's erasures, newest first.
func (s *Store) ListErasures(ctx context.Context, tenant string, limit int) ([]Erasure, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := []Erasure{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+erasureCols+` FROM erasures ORDER BY created_at DESC LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanErasure(rows)
			if err != nil {
				return err
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	return out, err
}

// PurgeAll runs Purge for every tenant.
func (s *Store) PurgeAll(ctx context.Context, maxAttempts int) (int, error) {
	tenants, err := s.ListTenants(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range tenants {
		k, err := s.Purge(ctx, t, maxAttempts)
		if err != nil {
			return n, err
		}
		n += k
	}
	return n, nil
}

// Purge works the pending and failed erasures of a tenant, domain by domain, each in its own
// transaction so a failure leaves the finished domains done. Returns how many erasures it touched.
func (s *Store) Purge(ctx context.Context, tenant string, maxAttempts int) (int, error) {
	var ids []string
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text FROM erasures WHERE status IN ('pending', 'running', 'failed') AND attempts < $1 ORDER BY created_at LIMIT 20`, maxAttempts)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := s.purgeOne(ctx, tenant, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

func (s *Store) purgeOne(ctx context.Context, tenant, id string) error {
	e, err := s.GetErasure(ctx, tenant, id)
	if err != nil {
		return err
	}
	if err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE erasures SET status = 'running', attempts = attempts + 1, updated_at = now() WHERE id = $1`, id)
		return err
	}); err != nil {
		return err
	}
	failed, transient := false, false
	var cause error
	for _, domain := range Domains {
		if d := e.Domains[domain]; d.Status == "done" {
			continue
		}
		rows, derr := s.purgeDomain(ctx, tenant, domain, e)
		d := Domain{Status: "done", Rows: rows, At: s.now().UTC()}
		if derr != nil {
			d = Domain{Status: "failed", At: s.now().UTC(), Error: derr.Error()}
			failed, transient, cause = true, blob.Transient(derr), derr
		}
		e.Domains[domain] = d
		raw, _ := json.Marshal(e.Domains)
		if err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE erasures SET domains = $2, updated_at = now() WHERE id = $1`, id, raw)
			return err
		}); err != nil {
			return err
		}
		if failed {
			break
		}
	}
	status := "verified"
	if failed {
		status = "failed"
	}
	if transient {
		s.noteWaiting(id, cause)
		// the object store is away or overloaded: that is not a failed attempt. The erasure stays pending, its attempt is
		// given back, and the worker tries again on its next round - however long the store is away (D54)
		return s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE erasures SET status = 'pending', attempts = greatest(attempts - 1, 0), updated_at = now() WHERE id = $1`, id)
			return err
		})
	}
	s.noteNotWaiting(id)
	return s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE erasures SET status = $2, updated_at = now() WHERE id = $1`, id, status); err != nil {
			return err
		}
		return Audit(ctx, tx, tenant, "idp-cmi/erasure", "erasure."+status, id, map[string]any{"records": len(e.RecordIDs), "documents": len(e.DocumentIDs)})
	})
}

// purgeDomain erases one domain for the erasure's targets; returns rows affected.
func (s *Store) purgeDomain(ctx context.Context, tenant, domain string, e *Erasure) (int, error) {
	if domain == "originals" {
		return s.deleteOriginals(ctx, tenant, e.DocumentIDs)
	}
	n := 0
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		count := func(q string, args ...any) error {
			tag, err := tx.Exec(ctx, q, args...)
			if err != nil {
				return err
			}
			n += int(tag.RowsAffected())
			return nil
		}
		switch domain {
		case "content":
			if err := count(`UPDATE memory_records SET content = $2, structured = NULL, tags = '{}', prov_ref = $2 WHERE id = ANY($1::uuid[])`, e.RecordIDs, Erased); err != nil {
				return err
			}
			var baseIDs []string
			if e.Selector.Subject != "" {
				rows, err := tx.Query(ctx, `SELECT id::text FROM knowledge_bases WHERE scope_kind = 'subject' AND scope_id = $1`, e.Selector.Subject)
				if err != nil {
					return err
				}
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						rows.Close()
						return err
					}
					baseIDs = append(baseIDs, id)
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					return err
				}
				if err := s.claimPersonalDocuments(ctx, tx, e); err != nil {
					return err
				}
				// a document the subject withdrew names them as the one who did
				if err := count(`UPDATE knowledge_documents SET revoked_by = $2 WHERE revoked_by = $1`, e.Selector.Subject, Erased); err != nil {
					return err
				}
				// the owner field of any document that names the subject
				if err := count(`UPDATE knowledge_documents SET owner = '' WHERE owner = $1`, e.Selector.Subject); err != nil {
					return err
				}
				// the personal base itself names the subject (its scope, and its name is a slug of the subject): it is renamed
				// to something derived from its id and closed. A member who is still around gets a new base on their next upload
				if err := count(`UPDATE knowledge_bases SET name = 'personal-erased-' || substr(replace(id::text, '-', ''), 1, 12), scope_id = 'erased-' || id::text,
					display_name = 'Erased', manager_subjects = '{}', created_by = $2, status = 'archived' WHERE id = ANY($1::uuid[])`, baseIDs, Erased); err != nil {
					return err
				}
			}
			// the conversions of the documents (their notices quote the document, their staged text is an object under the
			// document's prefix, which the originals domain sweeps) and the tickets that were presented for them (D69)
			if err := count(`DELETE FROM knowledge_conversions WHERE document_id = ANY($1::uuid[])`, e.DocumentIDs); err != nil {
				return err
			}
			if err := count(`DELETE FROM knowledge_ticket_uses WHERE document_id = ANY($1::uuid[])`, e.DocumentIDs); err != nil {
				return err
			}
			if err := count(`UPDATE knowledge_documents SET title = $2, owner = '' WHERE id = ANY($1::uuid[])`, e.DocumentIDs, Erased); err != nil {
				return err
			}
			// a document of a personal base keeps no path the member chose (it may say what the document is about): the uri
			// becomes kb://<base>/erased-<id>, and the maker's name goes
			if err := count(`UPDATE knowledge_documents d SET uri = 'kb://' || kb.name || '/erased-' || d.id::text, created_by = $2
				FROM knowledge_bases kb WHERE kb.id = d.knowledge_base_id AND kb.scope_kind = 'subject' AND d.id = ANY($1::uuid[])`, e.DocumentIDs, Erased); err != nil {
				return err
			}
			if err := count(`UPDATE knowledge_chunks SET content = $2, index_text = $2, heading_path = '', table_name = '', sheet = '', cell_range = '' WHERE document_id = ANY($1::uuid[])`, e.DocumentIDs, Erased); err != nil {
				return err
			}
			var left int
			if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM knowledge_conversions WHERE document_id = ANY($1::uuid[])) + (SELECT count(*) FROM knowledge_ticket_uses WHERE document_id = ANY($1::uuid[]))`, e.DocumentIDs).Scan(&left); err != nil {
				return err
			}
			if left > 0 { // a conversion that was queued after the barrier cannot happen (the document is revoked), but "verified" means gone
				return fmt.Errorf("%d conversion jobs or tickets of the erased documents are still there", left)
			}
			return s.verifyPersonalErased(ctx, tx, e, baseIDs)
		case "vectors":
			if err := count(`UPDATE memory_records SET embedding = NULL, embedding_model = 'erased' WHERE id = ANY($1::uuid[])`, e.RecordIDs); err != nil {
				return err
			}
			return count(`UPDATE knowledge_chunks SET embedding = NULL, embedding_model = 'erased' WHERE document_id = ANY($1::uuid[])`, e.DocumentIDs)
		case "index":
			if err := count(`UPDATE memory_records SET search = ''::tsvector WHERE id = ANY($1::uuid[])`, e.RecordIDs); err != nil {
				return err
			}
			return count(`UPDATE knowledge_chunks SET search = ''::tsvector WHERE document_id = ANY($1::uuid[])`, e.DocumentIDs)
		case "entities":
			return count(`DELETE FROM entity_refs WHERE (object_kind = 'memory' AND object_id = ANY($1::uuid[])) OR (object_kind = 'knowledge' AND object_id = ANY($2::uuid[]))`, e.RecordIDs, e.DocumentIDs)
		case "packages":
			if err := s.redactPackages(ctx, tx, e, &n); err != nil {
				return err
			}
			if e.Selector.Subject != "" {
				return s.redactSubjectPackages(ctx, tx, e, &n)
			}
			return nil
		case "audit":
			// audit rows keep action, subject, time and ids; the detail that may carry content is reduced
			return count(`UPDATE audit_events SET detail = jsonb_build_object('erased', true, 'erasureId', $2::text) WHERE resource_id = ANY($1::text[]) AND (action IN ('memory.create', 'knowledge.create') OR action LIKE 'knowledge.ingest.%')`,
				append(append([]string{}, e.RecordIDs...), e.DocumentIDs...), e.ID)
		}
		return fmt.Errorf("unknown domain %s", domain)
	})
	return n, err
}

// claimPersonalDocuments puts under the erasure's barrier the documents that reached the subject's personal base after the
// request was made, and records them with the erasure so every later domain covers them.
func (s *Store) claimPersonalDocuments(ctx context.Context, tx pgx.Tx, e *Erasure) error {
	rows, err := tx.Query(ctx, `UPDATE knowledge_documents d SET erasure_id = $1, revision = d.revision + 1, status = 'revoked',
		revoked_at = coalesce(d.revoked_at, now()), revoked_by = coalesce(d.revoked_by, $2), revoked_reason = coalesce(d.revoked_reason, 'erasure')
		FROM knowledge_bases kb WHERE kb.id = d.knowledge_base_id AND kb.scope_kind = 'subject' AND kb.scope_id = $3 AND d.erasure_id IS NULL
		RETURNING d.id::text`, e.ID, e.RequestedBy, e.Selector.Subject)
	if err != nil {
		return err
	}
	var late []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		late = append(late, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(late) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE erasures SET document_ids = document_ids || $2::uuid[] WHERE id = $1`, e.ID, late); err != nil {
		return err
	}
	e.DocumentIDs = append(e.DocumentIDs, late...)
	return nil
}

// verifyPersonalErased fails the content domain while any document of the subject's personal base is not under the
// erasure or any of its chunks still holds text: "verified" means they are gone.
func (s *Store) verifyPersonalErased(ctx context.Context, tx pgx.Tx, e *Erasure, baseIDs []string) error {
	if e.Selector.Subject == "" {
		return nil
	}
	var open, text int
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE d.erasure_id IS NULL), count(c.document_id) FILTER (WHERE c.content <> $2 OR c.index_text <> $2)
		FROM knowledge_documents d JOIN knowledge_bases kb ON kb.id = d.knowledge_base_id LEFT JOIN knowledge_chunks c ON c.document_id = d.id
		WHERE kb.id = ANY($1::uuid[])`, baseIDs, Erased).Scan(&open, &text); err != nil {
		return err
	}
	if open > 0 || text > 0 {
		return fmt.Errorf("the personal knowledge base of the subject still holds %d documents outside the erasure and %d chunks with text", open, text)
	}
	return nil
}

// redactSubjectPackages is for the erasure of a subject (D57): the packages they asked for keep neither the question nor
// who asked. The query and purpose become the tombstone, requestedBy (and the scope that names them) a marker that is the
// same in all of this erasure's packages and is derived from the erasure, not from the subject, so it cannot be
// recomputed from a guessed name. What the packages cited is redacted by redactPackages when it is theirs to erase.
func (s *Store) redactSubjectPackages(ctx context.Context, tx pgx.Tx, e *Erasure, n *int) error {
	marker := "erased-" + e.ID[:8]
	rows, err := tx.Query(ctx, `SELECT id::text, document FROM context_packages WHERE requested_by = $1`, e.Selector.Subject)
	if err != nil {
		return err
	}
	type pkg struct{ id, doc string }
	var pkgs []pkg
	for rows.Next() {
		var p pkg
		if err := rows.Scan(&p.id, &p.doc); err != nil {
			rows.Close()
			return err
		}
		pkgs = append(pkgs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rescope := func(m map[string]any) {
		if sc, ok := m["scope"].(map[string]any); ok && sc["id"] == e.Selector.Subject {
			sc["id"] = marker
		}
	}
	for _, p := range pkgs {
		var doc map[string]any
		if err := json.Unmarshal([]byte(p.doc), &doc); err != nil {
			return err
		}
		doc["query"], doc["requestedBy"] = Erased, marker
		if _, ok := doc["purpose"]; ok {
			doc["purpose"] = Erased
		}
		rescope(doc)
		items, _ := doc["items"].([]any)
		for _, it := range items {
			if m, ok := it.(map[string]any); ok {
				rescope(m)
			}
		}
		raw, err := canonical.Canonical(doc)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE context_packages SET document = $2, requested_by = $3, redacted = true WHERE id = $1`, p.id, string(raw), marker); err != nil {
			return err
		}
		*n++
	}
	return nil
}

// redactPackages rewrites the stored document of every package citing an erased item: the item's
// excerpt and title become the tombstone; the original digest stays as the record of what was
// sealed and redacted = true says the two no longer match.
func (s *Store) redactPackages(ctx context.Context, tx pgx.Tx, e *Erasure, n *int) error {
	targets := map[string]bool{}
	for _, id := range e.RecordIDs {
		targets[id] = true
	}
	for _, id := range e.DocumentIDs {
		targets[id] = true
	}
	all := append(append([]string{}, e.RecordIDs...), e.DocumentIDs...)
	uris := map[string]string{} // the uri a document has now (a personal one was rewritten by the content domain)
	if len(e.DocumentIDs) > 0 {
		ur, err := tx.Query(ctx, `SELECT id::text, uri FROM knowledge_documents WHERE id = ANY($1::uuid[])`, e.DocumentIDs)
		if err != nil {
			return err
		}
		for ur.Next() {
			var id, uri string
			if err := ur.Scan(&id, &uri); err != nil {
				ur.Close()
				return err
			}
			uris[id] = uri
		}
		ur.Close()
		if err := ur.Err(); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, `SELECT id::text, document FROM context_packages WHERE item_refs @> ANY(SELECT jsonb_build_array(jsonb_build_object('id', x)) FROM unnest($1::text[]) AS x)`, all)
	if err != nil {
		return err
	}
	type pkg struct{ id, doc string }
	var pkgs []pkg
	for rows.Next() {
		var p pkg
		if err := rows.Scan(&p.id, &p.doc); err != nil {
			rows.Close()
			return err
		}
		pkgs = append(pkgs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range pkgs {
		var doc map[string]any
		if err := json.Unmarshal([]byte(p.doc), &doc); err != nil {
			return err
		}
		items, _ := doc["items"].([]any)
		changed := false
		for _, it := range items {
			m, _ := it.(map[string]any)
			if id, _ := m["id"].(string); targets[id] {
				m["excerpt"] = Erased
				if ref, _ := m["ref"].(string); ref != "" && uris[id] != "" {
					if i := strings.LastIndex(ref, "#"); i >= 0 {
						m["ref"] = uris[id] + ref[i:]
					} else {
						m["ref"] = uris[id]
					}
				}
				if _, ok := m["title"]; ok {
					m["title"] = Erased
				}
				delete(m, "entities")
				changed = true
			}
		}
		if !changed {
			continue
		}
		raw, err := canonical.Canonical(doc)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE context_packages SET document = $2, redacted = true WHERE id = $1`, p.id, string(raw)); err != nil {
			return err
		}
		*n++
	}
	return nil
}
