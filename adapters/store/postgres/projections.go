package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// enqueueProjection records, inside the writer's transaction, that a row needs its vector projected
// (D26). kind is memory or chunk; version is the row's version / revision at this moment.
func enqueueProjection(ctx context.Context, tx pgx.Tx, tenant, kind, objectID string, seq, version int, action string) error {
	_, err := tx.Exec(ctx, `INSERT INTO projection_events (tenant_id, kind, object_id, seq, version, action) VALUES ($1, $2, $3, $4, $5, $6)`, tenant, kind, objectID, seq, version, action)
	return err
}

// ProjectionStatus is a tenant's projection backlog.
type ProjectionStatus struct {
	Pending          int            `json:"pending"`
	OldestAgeSeconds float64        `json:"oldestAgeSeconds"`
	ByKind           map[string]int `json:"byKind"`
	Model            string         `json:"model,omitempty"`
	CheckedAt        time.Time      `json:"checkedAt"`
}

// ProjectionBacklog reports a tenant's unprocessed projection events.
func (s *Store) ProjectionBacklog(ctx context.Context, tenant string) (*ProjectionStatus, error) {
	out := &ProjectionStatus{ByKind: map[string]int{}, CheckedAt: s.now().UTC()}
	if s.Embedder != nil {
		out.Model = s.Embedder.Model()
	}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT kind, count(*), coalesce(extract(epoch FROM now() - min(created_at)), 0) FROM projection_events WHERE processed_at IS NULL GROUP BY kind`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind string
			var n int
			var age float64
			if err := rows.Scan(&kind, &n, &age); err != nil {
				return err
			}
			out.ByKind[kind] = n
			out.Pending += n
			if age > out.OldestAgeSeconds {
				out.OldestAgeSeconds = age
			}
		}
		return rows.Err()
	})
	return out, err
}

// ProjectionLag is the age in seconds of a tenant's oldest unprocessed event (0 when none): what a
// package reports so a consumer knows the vector index may be behind the canonical rows.
func (s *Store) ProjectionLag(ctx context.Context, tenant string) (float64, error) {
	var lag float64
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(extract(epoch FROM now() - min(created_at)), 0) FROM projection_events WHERE processed_at IS NULL`).Scan(&lag)
	})
	return lag, err
}

// Project works up to limit pending events across tenants (D26): it embeds the rows the events name
// and writes the vectors only where the row is still at the event's version and still active; older
// events, deleted rows and non-active rows are marked processed without a write. Without an embedder
// nothing is processed and the backlog stays visible.
func (s *Store) Project(ctx context.Context, limit int) (int, error) {
	if s.Embedder == nil {
		return 0, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	model := s.Embedder.Model()
	n := 0
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.role', 'embedder', true)`); err != nil {
			return err
		}
		type ev struct {
			id             int64
			kind, objectID string
			seq, version   int
			action         string
			text           string
			apply          bool
		}
		rows, err := tx.Query(ctx, `SELECT id, kind, object_id::text, seq, version, action FROM projection_events WHERE processed_at IS NULL ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		var events []*ev
		for rows.Next() {
			e := &ev{}
			if err := rows.Scan(&e.id, &e.kind, &e.objectID, &e.seq, &e.version, &e.action); err != nil {
				rows.Close()
				return err
			}
			events = append(events, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		// what each event still applies to
		var texts []string
		var toEmbed []*ev
		for _, e := range events {
			if e.action != "upsert" {
				continue
			}
			var text string
			var current int
			var active bool
			var err error
			// D29: content above the tenant's external-model ceiling is never embedded; the event is done
			ceiling := sensitivityRankSQL("coalesce(tp.external_model_ceiling, 'internal')")
			if e.kind == "memory" {
				err = tx.QueryRow(ctx, `SELECT m.content, m.version, m.status = 'active' AND m.erasure_id IS NULL AND `+sensitivityRankSQL("m.sensitivity")+` <= `+ceiling+`
					FROM memory_records m LEFT JOIN tenant_policies tp ON tp.tenant_id = m.tenant_id WHERE m.id = $1`, e.objectID).Scan(&text, &current, &active)
			} else {
				err = tx.QueryRow(ctx, `SELECT c.content, d.revision, d.status = 'active' AND d.erasure_id IS NULL AND `+sensitivityRankSQL("d.sensitivity")+` <= `+ceiling+`
					FROM knowledge_chunks c JOIN knowledge_documents d ON d.id = c.document_id LEFT JOIN tenant_policies tp ON tp.tenant_id = d.tenant_id WHERE c.document_id = $1 AND c.seq = $2`, e.objectID, e.seq).Scan(&text, &current, &active)
			}
			if err != nil || !active || current != e.version {
				continue // gone, superseded by a newer version, or no longer active: nothing to project
			}
			e.apply = true
			e.text = text
			texts = append(texts, text)
			toEmbed = append(toEmbed, e)
		}
		var vecs [][]float32
		if len(texts) > 0 {
			vecs, err = s.Embedder.Embed(ctx, texts)
			if err != nil {
				// leave the events pending; count the attempt so a dead endpoint is visible
				ids := make([]int64, 0, len(events))
				for _, e := range events {
					ids = append(ids, e.id)
				}
				_, _ = tx.Exec(ctx, `UPDATE projection_events SET attempts = attempts + 1 WHERE id = ANY($1::bigint[])`, ids)
				return err
			}
		}
		b := &pgx.Batch{}
		for i, e := range toEmbed {
			if e.kind == "memory" {
				b.Queue(`UPDATE memory_records SET embedding = $2, embedding_model = $3 WHERE id = $1 AND version = $4`, e.objectID, vecs[i], model, e.version)
			} else {
				b.Queue(`UPDATE knowledge_chunks c SET embedding = $3, embedding_model = $4 FROM knowledge_documents d WHERE c.document_id = $1 AND c.seq = $2 AND d.id = c.document_id AND d.revision = $5`, e.objectID, e.seq, vecs[i], model, e.version)
			}
		}
		for _, e := range events {
			if e.action == "delete" {
				if e.kind == "memory" {
					b.Queue(`UPDATE memory_records SET embedding = NULL WHERE id = $1 AND status <> 'active'`, e.objectID)
				} else {
					b.Queue(`UPDATE knowledge_chunks c SET embedding = NULL FROM knowledge_documents d WHERE c.document_id = $1 AND d.id = c.document_id AND d.status <> 'active'`, e.objectID)
				}
			}
			b.Queue(`UPDATE projection_events SET processed_at = now(), attempts = attempts + 1 WHERE id = $1`, e.id)
		}
		if err := tx.SendBatch(ctx, b).Close(); err != nil {
			return err
		}
		n = len(events)
		return nil
	})
	return n, err
}
