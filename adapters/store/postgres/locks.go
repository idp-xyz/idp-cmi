package postgres

import (
	"context"
	"hash/fnv"
)

// TryWorkLock takes a session-level advisory lock named by kind and tenant on a connection of its
// own, so one replica at a time does work that must not run twice (consolidation, ADR-0001 §9 limit:
// "no mutual exclusion across replicas"). ok is false when another session holds it; release frees
// the lock and returns the connection. A crashed replica's lock goes with its connection.
func (s *Store) TryWorkLock(ctx context.Context, kind, tenant string) (release func(), ok bool, err error) {
	h := fnv.New64a()
	_, _ = h.Write([]byte("idp-cmi:" + kind + ":" + tenant))
	key := int64(h.Sum64())
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !ok {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		conn.Release()
	}, true, nil
}
