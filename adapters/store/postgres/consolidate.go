package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
	"idp-cmi/internal/guard"
)

// ListTenants returns every tenant id (background work iterates tenants and binds each in turn).
func (s *Store) ListTenants(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id::text FROM tenants ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ConsolidationGroup is a set of active, unconsolidated episodic records that share scope,
// sensitivity and ACL (D15): what one distillation reads.
type ConsolidationGroup struct {
	Scope       cmi.Scope
	Sensitivity string
	ACL         cmi.ACL
	Records     []cmi.MemoryRecord
}

// ConsolidationGroups returns the groups of a tenant with at least minRecords members (each capped
// at 50 records, oldest first); with scope set, only that scope.
func (s *Store) ConsolidationGroups(ctx context.Context, tenant string, minRecords int, scope *cmi.Scope) ([]ConsolidationGroup, error) {
	var out []ConsolidationGroup
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		kind, id := "", ""
		if scope != nil {
			kind, id = scope.Kind, scope.ID
		}
		rows, err := tx.Query(ctx, `SELECT scope_kind, scope_id, sensitivity, acl_subjects, acl_roles FROM memory_records
			WHERE kind = 'episodic' AND status = 'active' AND consolidation_id IS NULL AND (valid_until IS NULL OR valid_until > now()) AND cardinality(flags) = 0
			  AND ($2 = '' OR (scope_kind = $2 AND scope_id = $3))
			GROUP BY scope_kind, scope_id, sensitivity, acl_subjects, acl_roles HAVING count(*) >= $1 ORDER BY min(created_at)`, minRecords, kind, id)
		if err != nil {
			return err
		}
		var groups []ConsolidationGroup
		for rows.Next() {
			var g ConsolidationGroup
			if err := rows.Scan(&g.Scope.Kind, &g.Scope.ID, &g.Sensitivity, &g.ACL.Subjects, &g.ACL.Roles); err != nil {
				rows.Close()
				return err
			}
			groups = append(groups, g)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range groups {
			g := &groups[i]
			rows, err := tx.Query(ctx, `SELECT `+memoryCols+` FROM memory_records m JOIN sources s ON s.id = m.source_id
				WHERE m.kind = 'episodic' AND m.status = 'active' AND m.consolidation_id IS NULL AND (m.valid_until IS NULL OR m.valid_until > now()) AND cardinality(m.flags) = 0
				  AND m.scope_kind = $1 AND m.scope_id = $2 AND m.sensitivity = $3 AND m.acl_subjects = $4 AND m.acl_roles = $5 ORDER BY m.created_at LIMIT 50`,
				g.Scope.Kind, g.Scope.ID, g.Sensitivity, g.ACL.Subjects, g.ACL.Roles)
			if err != nil {
				return err
			}
			for rows.Next() {
				m, err := scanMemory(rows)
				if err != nil {
					rows.Close()
					return err
				}
				g.Records = append(g.Records, *m)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		out = groups
		return nil
	})
	return out, err
}

// Consolidation is one recorded distillation.
type Consolidation struct {
	ID          string          `json:"consolidationId"`
	Scope       cmi.Scope       `json:"scope"`
	Sensitivity string          `json:"sensitivity"`
	ACL         cmi.ACL         `json:"acl"`
	Model       string          `json:"model"`
	Status      string          `json:"status"` // completed | rejected (every statement flagged) | failed (model error)
	InputIDs    []string        `json:"inputIds"`
	OutputIDs   []string        `json:"outputIds"`
	Detail      json.RawMessage `json:"detail,omitempty"`
	CreatedBy   string          `json:"createdBy"`
	CreatedAt   time.Time       `json:"createdAt"`
}

// RecordedBy names the CMI itself as the writer of distilled statements.
const RecordedBy = "idp-cmi/consolidation"

// Consolidate writes the statements of one group as semantic records attributed to source (an agent
// source), marks the inputs as consumed and records the consolidation - in one transaction. Statements
// the write-time scan flags are dropped (D12) and listed in the detail; none left = rejected.
func (s *Store) Consolidate(ctx context.Context, tenant string, g ConsolidationGroup, source, model string, statements []string, modelErr error) (*Consolidation, error) {
	var out *Consolidation
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		policy, err := policyOf(ctx, tx, tenant)
		if err != nil {
			return err
		}
		inputIDs := make([]string, 0, len(g.Records))
		var observed time.Time
		var entities []cmi.Entity
		seen := map[cmi.Entity]bool{}
		for _, r := range g.Records {
			inputIDs = append(inputIDs, r.ID)
			if r.Provenance.ObservedAt.After(observed) {
				observed = r.Provenance.ObservedAt
			}
			for _, e := range r.Entities {
				if !seen[e] {
					seen[e] = true
					entities = append(entities, e)
				}
			}
		}
		id := NewUUID()
		c := &Consolidation{ID: id, Scope: g.Scope, Sensitivity: g.Sensitivity, ACL: g.ACL, Model: model, InputIDs: inputIDs, OutputIDs: []string{}, CreatedBy: RecordedBy}
		detail := map[string]any{"statements": len(statements)}
		var dropped []string
		if modelErr != nil {
			c.Status = "failed"
			detail["error"] = modelErr.Error()
		} else {
			derived, _ := json.Marshal(map[string]any{"derivedFrom": inputIDs, "consolidationId": id})
			now := s.now()
			for _, st := range statements {
				if flags := guard.Scan(st); len(flags) > 0 {
					dropped = append(dropped, st)
					continue
				}
				rec := cmi.MemoryRecord{Scope: g.Scope, Kind: cmi.KindSemantic, Content: st, Structured: derived, Tags: []string{"consolidated"},
					Provenance:  cmi.Provenance{Source: source, Ref: "consolidation:" + id, ObservedAt: observed, RecordedBy: RecordedBy},
					Validity:    cmi.Validity{ValidFrom: now, ValidUntil: now.Add(time.Duration(policy.ConsolidatedTTLDays) * 24 * time.Hour)},
					Sensitivity: g.Sensitivity, ACL: g.ACL, Entities: entities}
				m, err := s.insertMemory(ctx, tx, tenant, rec, RecordedBy, cmi.SensitivityRestricted)
				if err != nil {
					return err
				}
				c.OutputIDs = append(c.OutputIDs, m.ID)
			}
			if len(dropped) > 0 {
				detail["droppedByGuard"] = dropped
			}
			c.Status = "completed"
			if len(c.OutputIDs) == 0 {
				c.Status = "rejected"
			}
		}
		raw, _ := json.Marshal(detail)
		c.Detail = raw
		if _, err := tx.Exec(ctx, `INSERT INTO consolidations (id, tenant_id, scope_kind, scope_id, sensitivity, acl_subjects, acl_roles, model, status, input_ids, output_ids, detail, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::uuid[], $11::uuid[], $12, $13)`,
			id, tenant, g.Scope.Kind, g.Scope.ID, g.Sensitivity, g.ACL.Subjects, g.ACL.Roles, model, c.Status, inputIDs, c.OutputIDs, raw, RecordedBy); err != nil {
			return err
		}
		if c.Status != "failed" {
			if _, err := tx.Exec(ctx, `UPDATE memory_records SET consolidation_id = $1 WHERE id = ANY($2::uuid[]) AND consolidation_id IS NULL`, id, inputIDs); err != nil {
				return err
			}
		}
		if err := Audit(ctx, tx, tenant, RecordedBy, "memory.consolidate", id, map[string]any{"status": c.Status, "inputs": len(inputIDs), "outputs": len(c.OutputIDs), "dropped": len(dropped), "model": model}); err != nil {
			return err
		}
		c.CreatedAt = s.now()
		out = c
		return nil
	})
	return out, err
}

// ListConsolidations returns a tenant's consolidations, newest first.
func (s *Store) ListConsolidations(ctx context.Context, tenant string, limit int) ([]Consolidation, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := []Consolidation{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text, scope_kind, scope_id, sensitivity, acl_subjects, acl_roles, model, status, input_ids::text[], output_ids::text[], detail, created_by, created_at
			FROM consolidations ORDER BY created_at DESC LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c Consolidation
			var detail []byte
			if err := rows.Scan(&c.ID, &c.Scope.Kind, &c.Scope.ID, &c.Sensitivity, &c.ACL.Subjects, &c.ACL.Roles, &c.Model, &c.Status, &c.InputIDs, &c.OutputIDs, &detail, &c.CreatedBy, &c.CreatedAt); err != nil {
				return err
			}
			c.Detail = detail
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}
