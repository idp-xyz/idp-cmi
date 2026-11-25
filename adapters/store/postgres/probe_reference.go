package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/jackc/pgx/v5"
)

func probeDigest(probe string) string {
	sum := sha256.Sum256([]byte(probe))
	return hex.EncodeToString(sum[:])
}

// ProbeReference is the vector the documents endpoint gave for probe under the embedding model identity model (ADR-0001 D59),
// or nil when there is none: another model identity or another probe text has no reference of its own.
func (s *Store) ProbeReference(ctx context.Context, model, probe string) ([]float32, error) {
	var v []float32
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.role', 'embedder', true)`); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT vector FROM embedding_probe_references WHERE model = $1 AND probe_sha256 = $2`, model, probeDigest(probe)).Scan(&v)
		if err == pgx.ErrNoRows {
			v = nil
			return nil
		}
		return err
	})
	return v, err
}

// SaveProbeReference keeps v as the reference for (model, probe), replacing an earlier one.
func (s *Store) SaveProbeReference(ctx context.Context, model, probe string, v []float32) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.role', 'embedder', true)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO embedding_probe_references (model, probe_sha256, vector) VALUES ($1, $2, $3)
			ON CONFLICT (model, probe_sha256) DO UPDATE SET vector = EXCLUDED.vector, created_at = now()`, model, probeDigest(probe), v)
		return err
	})
}
