package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Evaluation is one run of the governance suite with its evidence (ADR-0001 D17).
type Evaluation struct {
	ID        string          `json:"evaluationId"`
	Status    string          `json:"status"` // PASSED | FAILED
	Checks    json.RawMessage `json:"checks"`
	CreatedBy string          `json:"createdBy"`
	CreatedAt time.Time       `json:"createdAt"`
}

// SaveEvaluation stores a run.
func (s *Store) SaveEvaluation(ctx context.Context, tenant, status string, checks any, by string) (*Evaluation, error) {
	raw, err := json.Marshal(checks)
	if err != nil {
		return nil, err
	}
	out := &Evaluation{ID: NewUUID(), Status: status, Checks: raw, CreatedBy: by}
	err = s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO evaluations (id, tenant_id, status, checks, created_by) VALUES ($1, $2, $3, $4, $5) RETURNING created_at`, out.ID, tenant, status, raw, by).Scan(&out.CreatedAt); err != nil {
			return err
		}
		return Audit(ctx, tx, tenant, by, "evaluation.run", out.ID, map[string]any{"status": status})
	})
	return out, err
}

const evalCols = `id::text, status, checks, created_by, created_at`

func scanEvaluation(row pgx.Row) (*Evaluation, error) {
	var e Evaluation
	var checks []byte
	if err := row.Scan(&e.ID, &e.Status, &checks, &e.CreatedBy, &e.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "evaluation not found")
		}
		return nil, err
	}
	e.Checks = checks
	return &e, nil
}

// GetEvaluation returns one run.
func (s *Store) GetEvaluation(ctx context.Context, tenant, id string) (*Evaluation, error) {
	var out *Evaluation
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = scanEvaluation(tx.QueryRow(ctx, `SELECT `+evalCols+` FROM evaluations WHERE id = $1`, id))
		return err
	})
	return out, err
}

// ListEvaluations returns a tenant's runs, newest first.
func (s *Store) ListEvaluations(ctx context.Context, tenant string, limit int) ([]Evaluation, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	out := []Evaluation{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+evalCols+` FROM evaluations ORDER BY created_at DESC LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEvaluation(rows)
			if err != nil {
				return err
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	return out, err
}
