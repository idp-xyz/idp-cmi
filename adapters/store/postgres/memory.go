package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
)

const memoryCols = `m.id::text, s.name, m.scope_kind, m.scope_id, m.kind, m.content, m.structured, m.tags, m.prov_ref, m.prov_observed_at, m.prov_recorded_by,
	m.valid_from, m.valid_until, m.status, m.revoked_at, coalesce(m.revoked_by, ''), coalesce(m.revoked_reason, ''), m.sensitivity, m.acl_subjects, m.acl_roles, m.created_at`

func scanMemory(row pgx.Row) (*cmi.MemoryRecord, error) {
	var m cmi.MemoryRecord
	var structured []byte
	var validUntil, revokedAt *time.Time
	var revokedBy, revokedReason string
	if err := row.Scan(&m.ID, &m.Provenance.Source, &m.Scope.Kind, &m.Scope.ID, &m.Kind, &m.Content, &structured, &m.Tags, &m.Provenance.Ref, &m.Provenance.ObservedAt, &m.Provenance.RecordedBy,
		&m.Validity.ValidFrom, &validUntil, &m.Status, &revokedAt, &revokedBy, &revokedReason, &m.Sensitivity, &m.ACL.Subjects, &m.ACL.Roles, &m.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "memory record not found")
		}
		return nil, err
	}
	if len(structured) > 0 {
		m.Structured = json.RawMessage(structured)
	}
	if validUntil != nil {
		m.Validity.ValidUntil = *validUntil
	}
	if revokedAt != nil {
		m.Revocation = &cmi.Revocation{At: *revokedAt, By: revokedBy, Reason: revokedReason}
	}
	m.Normalize()
	return &m, nil
}

// Visibility is the principal's view: who they are, their roles and their sensitivity ceiling.
type Visibility struct {
	Subject string
	Roles   []string
	Ceiling string
}

// visibleSQL is the predicate every read of memory and knowledge applies (D7): tenant rows only (RLS),
// usable, within the ceiling, and either unrestricted or naming the subject or one of the roles.
// Placeholders: $subject, $roles, $ceiling rank are passed as the first three arguments.
const visibleSQL = ` AND x.status = 'active' AND (x.valid_until IS NULL OR x.valid_until > now())
	AND (CASE x.sensitivity WHEN 'public' THEN 0 WHEN 'internal' THEN 1 WHEN 'confidential' THEN 2 ELSE 3 END) <= $3
	AND ((cardinality(x.acl_subjects) = 0 AND cardinality(x.acl_roles) = 0) OR $1 = ANY(x.acl_subjects) OR x.acl_roles && $2::text[])`

func rank(sensitivity string) int {
	return map[string]int{cmi.SensitivityPublic: 0, cmi.SensitivityInternal: 1, cmi.SensitivityConfidential: 2, cmi.SensitivityRestricted: 3}[sensitivity]
}

// CreateMemory appends a record. The writer's ceiling bounds the record's sensitivity; the policy's
// default TTL applies when no validUntil was given.
func (s *Store) CreateMemory(ctx context.Context, tenant string, in cmi.MemoryRecord, by string, ceiling string) (*cmi.MemoryRecord, error) {
	in.Normalize()
	if in.Validity.ValidFrom.IsZero() {
		in.Validity.ValidFrom = s.now()
	}
	if in.Provenance.RecordedBy == "" {
		in.Provenance.RecordedBy = by
	}
	if err := in.Validate(); err != nil {
		return nil, fail("INVALID", "%s", err)
	}
	if !cmi.Within(in.Sensitivity, ceiling) {
		return nil, fail("FORBIDDEN", "the writer may not record %s content (ceiling %s)", in.Sensitivity, ceiling)
	}
	var out *cmi.MemoryRecord
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		src, err := getSource(ctx, tx, in.Provenance.Source)
		if err != nil {
			return fail("CONFLICT", "source %s is not registered", in.Provenance.Source)
		}
		policy, err := policyOf(ctx, tx, tenant)
		if err != nil {
			return err
		}
		var validUntil any
		if !in.Validity.ValidUntil.IsZero() {
			validUntil = in.Validity.ValidUntil
		} else if policy.DefaultMemoryTTLDays > 0 {
			validUntil = in.Validity.ValidFrom.Add(time.Duration(policy.DefaultMemoryTTLDays) * 24 * time.Hour)
		}
		var structured any
		if len(in.Structured) > 0 {
			structured = []byte(in.Structured)
		}
		id := NewUUID()
		if _, err := tx.Exec(ctx, `INSERT INTO memory_records (id, tenant_id, source_id, scope_kind, scope_id, kind, content, structured, tags, prov_ref, prov_observed_at, prov_recorded_by,
			valid_from, valid_until, sensitivity, acl_subjects, acl_roles, search, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, to_tsvector('simple', $7) || to_tsvector('simple', $18), $19)`,
			id, tenant, src.ID, in.Scope.Kind, in.Scope.ID, in.Kind, in.Content, structured, in.Tags, in.Provenance.Ref, in.Provenance.ObservedAt, in.Provenance.RecordedBy,
			in.Validity.ValidFrom, validUntil, in.Sensitivity, in.ACL.Subjects, in.ACL.Roles, cmi.Bigrams(in.Content), by); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "memory.create", id, map[string]any{"source": src.Name, "ref": in.Provenance.Ref, "scope": in.Scope, "kind": in.Kind, "sensitivity": in.Sensitivity}); err != nil {
			return err
		}
		out, err = scanMemory(tx.QueryRow(ctx, `SELECT `+memoryCols+` FROM memory_records m JOIN sources s ON s.id = m.source_id WHERE m.id = $1`, id))
		return err
	})
	return out, err
}

// GetMemory returns a record the principal may see (any status: the owner of the view sees its
// history; the predicate on status is not applied here, only ACL and ceiling).
func (s *Store) GetMemory(ctx context.Context, tenant, id string, v Visibility) (*cmi.MemoryRecord, error) {
	var out *cmi.MemoryRecord
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = scanMemory(tx.QueryRow(ctx, `SELECT `+memoryCols+` FROM memory_records m JOIN sources s ON s.id = m.source_id WHERE m.id = $4
			AND (CASE m.sensitivity WHEN 'public' THEN 0 WHEN 'internal' THEN 1 WHEN 'confidential' THEN 2 ELSE 3 END) <= $3
			AND ((cardinality(m.acl_subjects) = 0 AND cardinality(m.acl_roles) = 0) OR $1 = ANY(m.acl_subjects) OR m.acl_roles && $2::text[])`,
			v.Subject, v.Roles, rank(v.Ceiling), id))
		return err
	})
	return out, err
}

// ListMemory lists the visible, usable records of a scope (newest first).
func (s *Store) ListMemory(ctx context.Context, tenant string, scope cmi.Scope, v Visibility, limit int) ([]cmi.MemoryRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := []cmi.MemoryRecord{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+memoryCols+` FROM memory_records m JOIN sources s ON s.id = m.source_id, LATERAL (SELECT m.status, m.valid_until, m.sensitivity, m.acl_subjects, m.acl_roles) x
			WHERE m.scope_kind = $4 AND m.scope_id = $5`+visibleSQL+` ORDER BY m.created_at DESC LIMIT $6`, v.Subject, v.Roles, rank(v.Ceiling), scope.Kind, scope.ID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMemory(rows)
			if err != nil {
				return err
			}
			out = append(out, *m)
		}
		return rows.Err()
	})
	return out, err
}

// RevokeMemory withdraws records by id or by provenance reference (D6). It returns how many were
// revoked. A writer may revoke what it recorded itself; an administrator anything.
func (s *Store) RevokeMemory(ctx context.Context, tenant, id, ref, by, reason string, admin bool) (int, error) {
	if (id == "") == (ref == "") {
		return 0, fail("INVALID", "give either recordId or ref")
	}
	n := 0
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		q := `UPDATE memory_records SET status = 'revoked', revoked_at = now(), revoked_by = $1, revoked_reason = $2 WHERE status <> 'revoked'`
		args := []any{by, reason}
		if id != "" {
			q += ` AND id = $3`
			args = append(args, id)
		} else {
			q += ` AND prov_ref = $3`
			args = append(args, ref)
		}
		if !admin {
			q += ` AND created_by = $1`
		}
		tag, err := tx.Exec(ctx, q, args...)
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		if n == 0 && id != "" {
			return fail("NOT_FOUND", "no revocable record %s for this principal", id)
		}
		return Audit(ctx, tx, tenant, by, "memory.revoke", id, map[string]any{"ref": ref, "reason": reason, "records": n})
	})
	return n, err
}

// memoryHit is a retrieval result.
type memoryHit struct {
	Record cmi.MemoryRecord
	Rank   float64
	Trust  string
}

// SearchMemory runs the full-text query over the records visible to v within the requested scopes.
func (s *Store) SearchMemory(ctx context.Context, tenant, tsquery string, scopes []cmi.Scope, v Visibility, limit int) ([]memoryHit, error) {
	var out []memoryHit
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		kinds := make([]string, 0, len(scopes))
		ids := make([]string, 0, len(scopes))
		for _, sc := range scopes {
			kinds = append(kinds, sc.Kind)
			ids = append(ids, sc.ID)
		}
		rows, err := tx.Query(ctx, `SELECT `+memoryCols+`, ts_rank(m.search, q) AS r, s.trust FROM memory_records m JOIN sources s ON s.id = m.source_id, to_tsquery('simple', $4) q,
			LATERAL (SELECT m.status, m.valid_until, m.sensitivity, m.acl_subjects, m.acl_roles) x
			WHERE m.search @@ q AND (m.scope_kind, m.scope_id) IN (SELECT unnest($5::text[]), unnest($6::text[]))`+visibleSQL+` ORDER BY r DESC LIMIT $7`,
			v.Subject, v.Roles, rank(v.Ceiling), tsquery, kinds, ids, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h memoryHit
			var structured []byte
			var validUntil, revokedAt *time.Time
			var revokedBy, revokedReason string
			m := &h.Record
			if err := rows.Scan(&m.ID, &m.Provenance.Source, &m.Scope.Kind, &m.Scope.ID, &m.Kind, &m.Content, &structured, &m.Tags, &m.Provenance.Ref, &m.Provenance.ObservedAt, &m.Provenance.RecordedBy,
				&m.Validity.ValidFrom, &validUntil, &m.Status, &revokedAt, &revokedBy, &revokedReason, &m.Sensitivity, &m.ACL.Subjects, &m.ACL.Roles, &m.CreatedAt, &h.Rank, &h.Trust); err != nil {
				return err
			}
			if validUntil != nil {
				m.Validity.ValidUntil = *validUntil
			}
			m.Normalize()
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}
