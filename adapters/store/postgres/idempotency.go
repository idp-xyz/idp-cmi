package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
)

// IdempotencyKeyRE bounds a caller's key.
var IdempotencyKeyRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// Idempotent runs write for a caller's key (ADR-0001 D19): the first call stores write's answer,
// a repeat with the same request replays it (replayed = true), a repeat with another request is a
// CONFLICT. Only successful answers (2xx) are recorded; a failed write may be retried with the key.
func (s *Store) Idempotent(ctx context.Context, tenant, subject, key, requestHash string, write func() (int, any, error)) (status int, body any, replayed bool, err error) {
	if !IdempotencyKeyRE.MatchString(key) {
		return 0, nil, false, fail("INVALID", "Idempotency-Key must match %s", IdempotencyKeyRE.String())
	}
	var stored []byte
	var storedHash string
	err = s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT request_sha256, status, response FROM idempotency_keys WHERE subject = $1 AND key = $2`, subject, key).Scan(&storedHash, &status, &stored)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return 0, nil, false, err
	}
	if stored != nil {
		if storedHash != requestHash {
			return 0, nil, false, fail("CONFLICT", "Idempotency-Key %s was used for a different request", key)
		}
		return status, json.RawMessage(stored), true, nil
	}
	status, body, err = write()
	if err != nil || status < 200 || status > 299 {
		return status, body, false, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, false, err
	}
	err = s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO idempotency_keys (tenant_id, subject, key, request_sha256, status, response) VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
			tenant, subject, key, requestHash, status, raw)
		return err
	})
	return status, json.RawMessage(raw), false, err
}
