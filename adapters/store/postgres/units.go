package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
)

const unitCols = `kind, id, display_name, parent_kind, parent_id, members, roles, updated_by, updated_at`

func scanUnit(row pgx.Row) (*cmi.Unit, error) {
	var u cmi.Unit
	var pk, pid *string
	if err := row.Scan(&u.Kind, &u.ID, &u.DisplayName, &pk, &pid, &u.Members, &u.Roles, &u.UpdatedBy, &u.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "unit not found")
		}
		return nil, err
	}
	if pk != nil && pid != nil {
		u.Parent = &cmi.Scope{Kind: *pk, ID: *pid}
	}
	return &u, nil
}

// PutUnit creates or replaces a unit of the directory (D22).
func (s *Store) PutUnit(ctx context.Context, tenant string, in cmi.Unit, by string) (*cmi.Unit, error) {
	if in.Members == nil {
		in.Members = []string{}
	}
	if in.Roles == nil {
		in.Roles = []string{}
	}
	if err := in.Validate(); err != nil {
		return nil, fail("INVALID", "%s", err)
	}
	var out *cmi.Unit
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var pk, pid any
		if in.Parent != nil {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM units WHERE kind = $1 AND id = $2`, in.Parent.Kind, in.Parent.ID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return fail("CONFLICT", "parent %s:%s is not registered", in.Parent.Kind, in.Parent.ID)
			}
			pk, pid = in.Parent.Kind, in.Parent.ID
		}
		if _, err := tx.Exec(ctx, `INSERT INTO units (tenant_id, kind, id, display_name, parent_kind, parent_id, members, roles, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (tenant_id, kind, id) DO UPDATE SET display_name = EXCLUDED.display_name, parent_kind = EXCLUDED.parent_kind, parent_id = EXCLUDED.parent_id,
			  members = EXCLUDED.members, roles = EXCLUDED.roles, updated_by = EXCLUDED.updated_by, updated_at = now()`,
			tenant, in.Kind, in.ID, in.DisplayName, pk, pid, in.Members, in.Roles, by); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "unit.put", in.Kind+":"+in.ID, map[string]any{"members": len(in.Members), "roles": in.Roles}); err != nil {
			return err
		}
		var err error
		out, err = scanUnit(tx.QueryRow(ctx, `SELECT `+unitCols+` FROM units WHERE kind = $1 AND id = $2`, in.Kind, in.ID))
		return err
	})
	return out, err
}

// DeleteUnit removes a unit; its memory stays but becomes unreachable until the unit is re-registered.
func (s *Store) DeleteUnit(ctx context.Context, tenant, kind, id, by string) error {
	return s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM units WHERE kind = $1 AND id = $2`, kind, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fail("NOT_FOUND", "unit not found")
		}
		return Audit(ctx, tx, tenant, by, "unit.delete", kind+":"+id, nil)
	})
}

// ListUnits returns the directory (administrators) by kind and id.
func (s *Store) ListUnits(ctx context.Context, tenant string) ([]cmi.Unit, error) {
	out := []cmi.Unit{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+unitCols+` FROM units ORDER BY kind, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			u, err := scanUnit(rows)
			if err != nil {
				return err
			}
			out = append(out, *u)
		}
		return rows.Err()
	})
	return out, err
}

// UnitsOf returns the unit scopes the principal belongs to: named as a member or holding one of the
// unit's roles. These are the scopes a resolve adds by default and the scopes it may write into.
func (s *Store) UnitsOf(ctx context.Context, tenant string, v Visibility) ([]cmi.Scope, error) {
	var out []cmi.Scope
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT kind, id FROM units WHERE $1 = ANY(members) OR roles && $2::text[] ORDER BY kind, id`, v.Subject, v.Roles)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sc cmi.Scope
			if err := rows.Scan(&sc.Kind, &sc.ID); err != nil {
				return err
			}
			out = append(out, sc)
		}
		return rows.Err()
	})
	return out, err
}

// IsMember reports whether the principal belongs to the unit (administrators always do).
func (s *Store) IsMember(ctx context.Context, tenant string, v Visibility, sc cmi.Scope, admin bool) (bool, error) {
	if admin {
		return true, nil
	}
	units, err := s.UnitsOf(ctx, tenant, v)
	if err != nil {
		return false, err
	}
	for _, u := range units {
		if u == sc {
			return true, nil
		}
	}
	return false, nil
}

// Share copies a record into a unit's scope (D22): a new active record with the same content,
// provenance and entities, sharedFrom naming the original, tagged shared. The original is untouched.
func (s *Store) Share(ctx context.Context, tenant, id string, to cmi.Scope, reason, by string, v Visibility) (*cmi.MemoryRecord, error) {
	if !to.IsUnit() || to.Validate() != nil {
		return nil, fail("INVALID", "share into an organization, team or project")
	}
	var out *cmi.MemoryRecord
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		src, err := scanMemory(tx.QueryRow(ctx, `SELECT `+memoryCols+` FROM memory_records m JOIN sources s ON s.id = m.source_id WHERE m.id = $1 AND m.erasure_id IS NULL`, id))
		if err != nil {
			return err
		}
		if src.Status != cmi.StatusActive {
			return fail("CONFLICT", "only an active record can be shared")
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM units WHERE kind = $1 AND id = $2`, to.Kind, to.ID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fail("NOT_FOUND", "unit %s:%s is not registered", to.Kind, to.ID)
		}
		copyRec := *src
		copyRec.ID, copyRec.Scope, copyRec.Status, copyRec.Version, copyRec.Flags, copyRec.DisputedBy = "", to, cmi.StatusActive, 0, nil, ""
		copyRec.Tags = append(append([]string{}, src.Tags...), "shared")
		copyRec.Validity = cmi.Validity{ValidFrom: s.now(), ValidUntil: src.Validity.ValidUntil}
		copyRec.Revocation = nil
		out, err = s.insertMemory(ctx, tx, tenant, copyRec, by, cmi.SensitivityRestricted)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE memory_records SET shared_from = $2 WHERE id = $1`, out.ID, id); err != nil {
			return err
		}
		out.SharedFrom = id
		_ = v
		return Audit(ctx, tx, tenant, by, "memory.share", out.ID, map[string]any{"from": id, "to": to, "reason": reason})
	})
	return out, err
}

var _ = time.Now
