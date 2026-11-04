package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
)

// EntityView is what refers to one entity, as the principal may see it (D13).
type EntityView struct {
	Entity    cmi.Entity         `json:"entity"`
	Records   []cmi.MemoryRecord `json:"records"`
	Documents []cmi.Document     `json:"documents"`
}

// ListByEntity returns the visible, usable records and documents referencing the entity.
func (s *Store) ListByEntity(ctx context.Context, tenant string, e cmi.Entity, v Visibility, limit int) (*EntityView, error) {
	if err := cmi.ValidateEntities([]cmi.Entity{e}); err != nil {
		return nil, fail("INVALID", "%s", err)
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := &EntityView{Entity: e, Records: []cmi.MemoryRecord{}, Documents: []cmi.Document{}}
	key := []string{e.Type + ":" + e.ID}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+memoryCols+` FROM memory_records m JOIN sources s ON s.id = m.source_id, LATERAL (SELECT m.status, m.valid_until, m.sensitivity, m.acl_subjects, m.acl_roles) x
			WHERE true`+visibleSQL+entityFilterSQL("memory", "m.id", "$4")+` ORDER BY m.created_at DESC LIMIT $5`, v.Subject, v.Roles, rank(v.Ceiling), key, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			m, err := scanMemory(rows)
			if err != nil {
				rows.Close()
				return err
			}
			out.Records = append(out.Records, *m)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT `+docCols+` FROM knowledge_documents d JOIN sources s ON s.id = d.source_id, LATERAL (SELECT d.status, d.valid_until, d.sensitivity, d.acl_subjects, d.acl_roles) x
			WHERE true`+visibleSQL+entityFilterSQL("knowledge", "d.id", "$4")+` ORDER BY d.created_at DESC LIMIT $5`, v.Subject, v.Roles, rank(v.Ceiling), key, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDoc(rows)
			if err != nil {
				return err
			}
			out.Documents = append(out.Documents, *d)
		}
		return rows.Err()
	})
	return out, err
}
