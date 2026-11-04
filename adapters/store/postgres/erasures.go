package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

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
	Subject     string      `json:"subject,omitempty"` // records in that subject's scope
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
var Domains = []string{"content", "vectors", "index", "entities", "packages", "audit"}

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
	failed := false
	for _, domain := range Domains {
		if d := e.Domains[domain]; d.Status == "done" {
			continue
		}
		rows, derr := s.purgeDomain(ctx, tenant, domain, e)
		d := Domain{Status: "done", Rows: rows, At: s.now().UTC()}
		if derr != nil {
			d = Domain{Status: "failed", At: s.now().UTC(), Error: derr.Error()}
			failed = true
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
	return s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE erasures SET status = $2, updated_at = now() WHERE id = $1`, id, status); err != nil {
			return err
		}
		return Audit(ctx, tx, tenant, "idp-cmi/erasure", "erasure."+status, id, map[string]any{"records": len(e.RecordIDs), "documents": len(e.DocumentIDs)})
	})
}

// purgeDomain erases one domain for the erasure's targets; returns rows affected.
func (s *Store) purgeDomain(ctx context.Context, tenant, domain string, e *Erasure) (int, error) {
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
			if err := count(`UPDATE knowledge_documents SET title = $2 WHERE id = ANY($1::uuid[])`, e.DocumentIDs, Erased); err != nil {
				return err
			}
			return count(`UPDATE knowledge_chunks SET content = $2 WHERE document_id = ANY($1::uuid[])`, e.DocumentIDs, Erased)
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
			return s.redactPackages(ctx, tx, e, &n)
		case "audit":
			// audit rows keep action, subject, time and ids; the detail that may carry content is reduced
			return count(`UPDATE audit_events SET detail = jsonb_build_object('erased', true, 'erasureId', $2::text) WHERE resource_id = ANY($1::text[]) AND action IN ('memory.create', 'knowledge.create')`,
				append(append([]string{}, e.RecordIDs...), e.DocumentIDs...), e.ID)
		}
		return fmt.Errorf("unknown domain %s", domain)
	})
	return n, err
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
