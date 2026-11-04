package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
)

// ItemRef names a record or document a package cited.
type ItemRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// StoredPackage is a package as read back: the sealed envelope plus what went stale since (D5, D6).
type StoredPackage struct {
	Envelope  cmi.Envelope `json:"envelope"`
	Stale     bool         `json:"stale"`
	StaleIDs  []string     `json:"staleItems"`
	ExpiredAt time.Time    `json:"expiresAt"`
	Redacted  bool         `json:"redacted"` // D20: erased items' plaintext was removed; the digest is of the sealed original
}

// SavePackage stores a sealed package.
func (s *Store) SavePackage(ctx context.Context, tenant string, env *cmi.Envelope, p *cmi.Package) error {
	refs := make([]ItemRef, 0, len(p.Items))
	for _, it := range p.Items {
		refs = append(refs, ItemRef{Kind: it.Kind, ID: it.ID})
	}
	raw, _ := json.Marshal(refs)
	return s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO context_packages (id, tenant_id, requested_by, digest, document, item_refs, expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			p.ID, tenant, p.RequestedBy, env.Digest, string(env.Document), raw, p.ExpiresAt)
		return err
	})
}

// GetPackage reads a package back for the principal that requested it (or an administrator) and
// reports which cited items have since expired, been revoked or superseded.
func (s *Store) GetPackage(ctx context.Context, tenant, id, requester string) (*StoredPackage, error) {
	var out StoredPackage
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var doc string
		var refs []byte
		if err := tx.QueryRow(ctx, `SELECT digest, document, item_refs, expires_at, redacted FROM context_packages WHERE id = $1 AND ($2 = '' OR requested_by = $2)`, id, requester).
			Scan(&out.Envelope.Digest, &doc, &refs, &out.ExpiredAt, &out.Redacted); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fail("NOT_FOUND", "package not found")
			}
			return err
		}
		out.Envelope.Document = json.RawMessage(doc)
		var items []ItemRef
		if err := json.Unmarshal(refs, &items); err != nil {
			return err
		}
		out.StaleIDs = []string{}
		for _, it := range items {
			var ok bool
			var err error
			switch it.Kind {
			case cmi.ItemMemory:
				err = tx.QueryRow(ctx, `SELECT status = 'active' AND (valid_until IS NULL OR valid_until > now()) FROM memory_records WHERE id = $1`, it.ID).Scan(&ok)
			default:
				err = tx.QueryRow(ctx, `SELECT status = 'active' AND (valid_until IS NULL OR valid_until > now()) FROM knowledge_documents WHERE id = $1`, it.ID).Scan(&ok)
			}
			if errors.Is(err, pgx.ErrNoRows) {
				ok = false
			} else if err != nil {
				return err
			}
			if !ok {
				out.Stale = true
				out.StaleIDs = append(out.StaleIDs, it.ID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// VerifyPackage reports the current state of every item a package cites (D24). The requester (or
// an administrator, requester "") may ask; the package's validity is not extended.
func (s *Store) VerifyPackage(ctx context.Context, tenant, id, requester string) (*cmi.Verification, error) {
	out := &cmi.Verification{PackageID: id, StaleItems: []string{}, Items: []cmi.ItemStatus{}, VerifiedAt: s.now().UTC().Truncate(time.Second)}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var refs []byte
		if err := tx.QueryRow(ctx, `SELECT item_refs, expires_at, redacted FROM context_packages WHERE id = $1 AND ($2 = '' OR requested_by = $2)`, id, requester).Scan(&refs, &out.ExpiresAt, &out.Redacted); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fail("NOT_FOUND", "package not found")
			}
			return err
		}
		var items []ItemRef
		if err := json.Unmarshal(refs, &items); err != nil {
			return err
		}
		for _, it := range items {
			st := cmi.ItemStatus{Kind: it.Kind, ID: it.ID, Status: "missing"}
			var err error
			if it.Kind == cmi.ItemMemory {
				err = tx.QueryRow(ctx, `SELECT status, version, status = 'active' AND (valid_until IS NULL OR valid_until > now()) AND erasure_id IS NULL FROM memory_records WHERE id = $1`, it.ID).Scan(&st.Status, &st.Version, &st.Usable)
			} else {
				err = tx.QueryRow(ctx, `SELECT status, revision, status = 'active' AND (valid_until IS NULL OR valid_until > now()) AND erasure_id IS NULL FROM knowledge_documents WHERE id = $1`, it.ID).Scan(&st.Status, &st.Version, &st.Usable)
			}
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if !st.Usable {
				out.Stale = true
				out.StaleItems = append(out.StaleItems, it.ID)
			}
			out.Items = append(out.Items, st)
		}
		out.ExpiresAt = out.ExpiresAt.UTC()
		return nil
	})
	return out, err
}

// Sweep marks records and documents past their validity as expired (D6). It runs across tenants
// under the sweeper policy and returns how many rows changed.
func (s *Store) Sweep(ctx context.Context) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.role', 'sweeper', true)`); err != nil {
			return err
		}
		a, err := tx.Exec(ctx, `UPDATE memory_records SET status = 'expired', version = version + 1 WHERE status = 'active' AND valid_until IS NOT NULL AND valid_until <= now()`)
		if err != nil {
			return err
		}
		b, err := tx.Exec(ctx, `UPDATE knowledge_documents SET status = 'expired', revision = revision + 1 WHERE status = 'active' AND valid_until IS NOT NULL AND valid_until <= now()`)
		if err != nil {
			return err
		}
		// idempotency keys outlive their use by a day (D19)
		if _, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < now() - interval '24 hours'`); err != nil {
			return err
		}
		n = int(a.RowsAffected() + b.RowsAffected())
		return nil
	})
	return n, err
}
