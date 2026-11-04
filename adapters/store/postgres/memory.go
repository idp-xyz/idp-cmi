package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"idp-cmi/internal/guard"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
)

const memoryCols = `m.id::text, s.name, m.scope_kind, m.scope_id, m.kind, CASE WHEN m.erasure_id IS NULL THEN m.content ELSE '[erased]' END, CASE WHEN m.erasure_id IS NULL THEN m.structured ELSE NULL END, m.tags, CASE WHEN m.erasure_id IS NULL THEN m.prov_ref ELSE '[erased]' END, m.prov_observed_at, m.prov_recorded_by,
	m.valid_from, m.valid_until, m.status, m.revoked_at, coalesce(m.revoked_by, ''), coalesce(m.revoked_reason, ''), m.sensitivity, m.acl_subjects, m.acl_roles, m.created_at,
	m.flags, m.version, ` + "(SELECT coalesce(json_agg(json_build_object('type', e.entity_type, 'id', e.entity_id) ORDER BY e.entity_type, e.entity_id), '[]')::text FROM entity_refs e WHERE e.object_kind = 'memory' AND e.object_id = m.id)"

func scanMemory(row pgx.Row) (*cmi.MemoryRecord, error) {
	var m cmi.MemoryRecord
	var structured []byte
	var validUntil, revokedAt *time.Time
	var revokedBy, revokedReason, entities string
	if err := row.Scan(&m.ID, &m.Provenance.Source, &m.Scope.Kind, &m.Scope.ID, &m.Kind, &m.Content, &structured, &m.Tags, &m.Provenance.Ref, &m.Provenance.ObservedAt, &m.Provenance.RecordedBy,
		&m.Validity.ValidFrom, &validUntil, &m.Status, &revokedAt, &revokedBy, &revokedReason, &m.Sensitivity, &m.ACL.Subjects, &m.ACL.Roles, &m.CreatedAt, &m.Flags, &m.Version, &entities); err != nil {
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
	m.Entities = decodeEntities(entities)
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
	var out *cmi.MemoryRecord
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = s.insertMemory(ctx, tx, tenant, in, by, ceiling)
		return err
	})
	return out, err
}

// insertMemory is CreateMemory inside a caller's transaction (consolidation writes its statements
// and marks its inputs atomically, D15). The content is scanned (D12) and embedded (D11) here.
func (s *Store) insertMemory(ctx context.Context, tx pgx.Tx, tenant string, in cmi.MemoryRecord, by string, ceiling string) (*cmi.MemoryRecord, error) {
	in.Normalize()
	if in.Validity.ValidFrom.IsZero() {
		in.Validity.ValidFrom = s.now()
	}
	if in.Provenance.RecordedBy == "" {
		in.Provenance.RecordedBy = by
	}
	in.Flags = guard.Scan(in.Content) // D12: the writer's own flags are ignored
	if err := in.Validate(); err != nil {
		return nil, fail("INVALID", "%s", err)
	}
	if !cmi.Within(in.Sensitivity, ceiling) {
		return nil, fail("FORBIDDEN", "the writer may not record %s content (ceiling %s)", in.Sensitivity, ceiling)
	}
	src, err := getSource(ctx, tx, in.Provenance.Source)
	if err != nil {
		return nil, fail("CONFLICT", "source %s is not registered", in.Provenance.Source)
	}
	policy, err := policyOf(ctx, tx, tenant)
	if err != nil {
		return nil, err
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
	vecs, model := s.embed(ctx, []string{in.Content})
	var vec, vecModel any
	if vecs != nil {
		vec, vecModel = vecs[0], model
	}
	flags := in.Flags
	if flags == nil {
		flags = []string{}
	}
	id := NewUUID()
	if _, err := tx.Exec(ctx, `INSERT INTO memory_records (id, tenant_id, source_id, scope_kind, scope_id, kind, content, structured, tags, prov_ref, prov_observed_at, prov_recorded_by,
		valid_from, valid_until, sensitivity, acl_subjects, acl_roles, search, created_by, flags, embedding, embedding_model)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, to_tsvector('simple', $7) || to_tsvector('simple', $18), $19, $20, $21, $22)`,
		id, tenant, src.ID, in.Scope.Kind, in.Scope.ID, in.Kind, in.Content, structured, in.Tags, in.Provenance.Ref, in.Provenance.ObservedAt, in.Provenance.RecordedBy,
		in.Validity.ValidFrom, validUntil, in.Sensitivity, in.ACL.Subjects, in.ACL.Roles, cmi.Bigrams(in.Content), by, flags, vec, vecModel); err != nil {
		return nil, err
	}
	if err := insertEntities(ctx, tx, tenant, "memory", id, in.Entities); err != nil {
		return nil, err
	}
	if err := Audit(ctx, tx, tenant, by, "memory.create", id, map[string]any{"source": src.Name, "ref": in.Provenance.Ref, "scope": in.Scope, "kind": in.Kind, "sensitivity": in.Sensitivity, "flags": flags, "entities": len(in.Entities)}); err != nil {
		return nil, err
	}
	return scanMemory(tx.QueryRow(ctx, `SELECT `+memoryCols+` FROM memory_records m JOIN sources s ON s.id = m.source_id WHERE m.id = $1`, id))
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

// ListMemory lists the visible, usable records of a scope (newest first). With flagged, only
// records the write-time scan flagged are listed (D12), across scopes when scope.Kind is empty.
func (s *Store) ListMemory(ctx context.Context, tenant string, scope cmi.Scope, v Visibility, limit int, flagged bool) ([]cmi.MemoryRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := []cmi.MemoryRecord{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+memoryCols+` FROM memory_records m JOIN sources s ON s.id = m.source_id, LATERAL (SELECT m.status, m.valid_until, m.sensitivity, m.acl_subjects, m.acl_roles) x
			WHERE ($4 = '' OR (m.scope_kind = $4 AND m.scope_id = $5)) AND (NOT $7 OR cardinality(m.flags) > 0)`+visibleSQL+` ORDER BY m.created_at DESC LIMIT $6`,
			v.Subject, v.Roles, rank(v.Ceiling), scope.Kind, scope.ID, limit, flagged)
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
func (s *Store) RevokeMemory(ctx context.Context, tenant, id, ref, by, reason string, admin bool, ifMatch int) (int, error) {
	if (id == "") == (ref == "") {
		return 0, fail("INVALID", "give either recordId or ref")
	}
	if ifMatch != 0 && id == "" {
		return 0, fail("INVALID", "If-Match applies to a single recordId")
	}
	n := 0
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		q := `UPDATE memory_records SET status = 'revoked', revoked_at = now(), revoked_by = $1, revoked_reason = $2, version = version + 1 WHERE status <> 'revoked'`
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
		if ifMatch != 0 {
			q += ` AND version = $4`
			args = append(args, ifMatch)
		}
		tag, err := tx.Exec(ctx, q, args...)
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		if n == 0 && id != "" {
			if ifMatch != 0 {
				var current int
				if err := tx.QueryRow(ctx, `SELECT version FROM memory_records WHERE id = $1`, id).Scan(&current); err == nil && current != ifMatch {
					return fail("CONFLICT", "record %s is at version %d, not %d", id, current, ifMatch)
				}
			}
			return fail("NOT_FOUND", "no revocable record %s for this principal", id)
		}
		return Audit(ctx, tx, tenant, by, "memory.revoke", id, map[string]any{"ref": ref, "reason": reason, "records": n})
	})
	return n, err
}

// memoryHit is a retrieval result.
type memoryHit struct {
	Record cmi.MemoryRecord
	Trust  string
	Hit
}

// SearchMemory retrieves the records visible to v within the requested scopes: lexically over the
// full-text index and semantically over the stored vectors (D11), merged per record.
func (s *Store) SearchMemory(ctx context.Context, tenant string, q Query, scopes []cmi.Scope, v Visibility) ([]memoryHit, error) {
	kinds := make([]string, 0, len(scopes))
	ids := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		kinds = append(kinds, sc.Kind)
		ids = append(ids, sc.ID)
	}
	entities := q.Entities
	if entities == nil {
		entities = []string{}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	const from = ` FROM memory_records m JOIN sources s ON s.id = m.source_id, LATERAL (SELECT m.status, m.valid_until, m.sensitivity, m.acl_subjects, m.acl_roles) x`
	filters := ` AND (m.scope_kind, m.scope_id) IN (SELECT unnest($5::text[]), unnest($6::text[]))` + visibleSQL + ` AND ($7 OR cardinality(m.flags) = 0)` + entityFilterSQL("memory", "m.id", "$8")
	byID := map[string]*memoryHit{}
	var order []string
	scan := func(rows pgx.Rows, semantic bool) error {
		defer rows.Close()
		for rows.Next() {
			var h memoryHit
			var structured []byte
			var validUntil, revokedAt *time.Time
			var revokedBy, revokedReason, entities string
			var r float64
			m := &h.Record
			if err := rows.Scan(&m.ID, &m.Provenance.Source, &m.Scope.Kind, &m.Scope.ID, &m.Kind, &m.Content, &structured, &m.Tags, &m.Provenance.Ref, &m.Provenance.ObservedAt, &m.Provenance.RecordedBy,
				&m.Validity.ValidFrom, &validUntil, &m.Status, &revokedAt, &revokedBy, &revokedReason, &m.Sensitivity, &m.ACL.Subjects, &m.ACL.Roles, &m.CreatedAt, &m.Flags, &m.Version, &entities, &r, &h.Trust); err != nil {
				return err
			}
			if validUntil != nil {
				m.Validity.ValidUntil = *validUntil
			}
			m.Entities = decodeEntities(entities)
			m.Normalize()
			hit := Hit{}
			if semantic {
				hit.Semantic = r
			} else {
				hit.Lexical = r
			}
			if ex := byID[m.ID]; ex != nil {
				ex.merge(hit)
				continue
			}
			h.merge(hit)
			byID[m.ID] = &h
			order = append(order, m.ID)
		}
		return rows.Err()
	}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if q.TSQuery != "" {
			rows, err := tx.Query(ctx, `SELECT `+memoryCols+`, ts_rank(m.search, q)::float8 AS r, s.trust`+from+`, to_tsquery('simple', $4) q
				WHERE m.search @@ q`+filters+` ORDER BY r DESC LIMIT $9`, v.Subject, v.Roles, rank(v.Ceiling), q.TSQuery, kinds, ids, q.IncludeFlagged, entities, limit)
			if err != nil {
				return err
			}
			if err := scan(rows, false); err != nil {
				return err
			}
		}
		if len(q.Vector) > 0 && q.Model != "" {
			rows, err := tx.Query(ctx, `SELECT `+memoryCols+`, cosine_similarity(m.embedding, $4::real[])::float8 AS r, s.trust`+from+`
				WHERE m.embedding IS NOT NULL AND m.embedding_model = $10`+filters+` ORDER BY r DESC NULLS LAST LIMIT $9`, v.Subject, v.Roles, rank(v.Ceiling), q.Vector, kinds, ids, q.IncludeFlagged, entities, limit, q.Model)
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
	out := make([]memoryHit, 0, len(order))
	for _, id := range order {
		if h := byID[id]; h.Semantic > 0 || h.Lexical > 0 {
			out = append(out, *h)
		}
	}
	return out, nil
}
