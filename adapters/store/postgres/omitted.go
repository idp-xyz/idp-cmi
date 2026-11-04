package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
)

// Excluded counts lexical matches the usable predicate dropped for reportable reasons (D28): the
// caller may see these rows (ACL and ceiling apply) but a status, a flag or a policy obligation keeps
// them out of packages. Vector-only matches are not counted here.
func (s *Store) Excluded(ctx context.Context, tenant string, q Query, scopes []cmi.Scope, sources []string, v Visibility) (cmi.Omitted, error) {
	var out cmi.Omitted
	if q.TSQuery == "" {
		return out, nil
	}
	kinds := make([]string, 0, len(scopes))
	ids := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		kinds = append(kinds, sc.Kind)
		ids = append(ids, sc.ID)
	}
	if sources == nil {
		sources = []string{}
	}
	entities := q.Entities
	if entities == nil {
		entities = []string{}
	}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var cand, disp, flagged, forbidden int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE m.status = 'candidate'), count(*) FILTER (WHERE m.status = 'disputed'),
			count(*) FILTER (WHERE m.status = 'active' AND cardinality(m.flags) > 0), count(*) FILTER (WHERE m.status = 'active' AND cardinality(m.flags) = 0 AND s.name = ANY($8::text[]))
			FROM memory_records m JOIN sources s ON s.id = m.source_id, to_tsquery('simple', $4) q, LATERAL (SELECT m.status, m.valid_until, m.sensitivity, m.acl_subjects, m.acl_roles) x
			WHERE m.search @@ q AND m.erasure_id IS NULL AND (m.valid_until IS NULL OR m.valid_until > now()) AND (m.scope_kind, m.scope_id) IN (SELECT unnest($5::text[]), unnest($6::text[]))`+aclSQL+entityFilterSQL("memory", "m.id", "$7"),
			v.Subject, v.Roles, rank(v.Ceiling), q.TSQuery, kinds, ids, entities, excluded(v)).Scan(&cand, &disp, &flagged, &forbidden); err != nil {
			return err
		}
		out.Candidate, out.Disputed, out.Flagged, out.Forbidden = cand, disp, flagged, forbidden
		var kFlagged, kForbidden int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE cardinality(c.flags) > 0), count(*) FILTER (WHERE cardinality(c.flags) = 0 AND s.name = ANY($7::text[]))
			FROM knowledge_chunks c JOIN knowledge_documents d ON d.id = c.document_id JOIN sources s ON s.id = d.source_id, to_tsquery('simple', $4) q, LATERAL (SELECT d.status, d.valid_until, d.sensitivity, d.acl_subjects, d.acl_roles) x
			WHERE c.search @@ q AND d.status = 'active' AND d.erasure_id IS NULL AND (d.valid_until IS NULL OR d.valid_until > now()) AND (cardinality($5::text[]) = 0 OR s.name = ANY($5))`+aclSQL+entityFilterSQL("knowledge", "d.id", "$6"),
			v.Subject, v.Roles, rank(v.Ceiling), q.TSQuery, sources, entities, excluded(v)).Scan(&kFlagged, &kForbidden); err != nil {
			return err
		}
		out.Flagged += kFlagged
		out.Forbidden += kForbidden
		return nil
	})
	return out, err
}
