package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// LiveCounts returns how many live records (active, candidate, disputed: what still occupies the
// store) one scope and the whole tenant hold. Used by the quota check before a write (a soft limit:
// the count and the insert are separate statements).
func (s *Store) LiveCounts(ctx context.Context, tenant, scopeKind, scopeID string) (scope, total int, err error) {
	err = s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE scope_kind = $1 AND scope_id = $2),
			count(*)
			FROM memory_records WHERE status IN ('active', 'candidate', 'disputed')`, scopeKind, scopeID).Scan(&scope, &total)
	})
	return scope, total, err
}

// ProjectionTotals is the projection backlog across all tenants (the embedder role reads every
// tenant's outbox): the pending count and the age in seconds of the oldest pending event.
func (s *Store) ProjectionTotals(ctx context.Context) (pending int, oldestAgeSeconds float64, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.role', 'embedder', true)`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*), coalesce(extract(epoch FROM now() - min(created_at)), 0) FROM projection_events WHERE processed_at IS NULL`).Scan(&pending, &oldestAgeSeconds)
	})
	return pending, oldestAgeSeconds, err
}
