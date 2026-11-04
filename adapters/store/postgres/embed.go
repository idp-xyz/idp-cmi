package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// BackfillEmbeddings vectorizes up to limit records and chunks that have no vector of the current
// model (D11): rows written while the endpoint was down or before it was configured, and rows
// embedded by a previous model. Runs across tenants under the embedder policy; returns rows updated.
func (s *Store) BackfillEmbeddings(ctx context.Context, limit int) (int, error) {
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
		type row struct {
			id   string
			seq  int
			text string
		}
		var mem, chunks []row
		rows, err := tx.Query(ctx, `SELECT id::text, content FROM memory_records WHERE status = 'active' AND (embedding IS NULL OR embedding_model IS DISTINCT FROM $1) ORDER BY created_at LIMIT $2`, model, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.text); err != nil {
				rows.Close()
				return err
			}
			mem = append(mem, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT c.document_id::text, c.seq, c.content FROM knowledge_chunks c JOIN knowledge_documents d ON d.id = c.document_id
			WHERE d.status = 'active' AND (c.embedding IS NULL OR c.embedding_model IS DISTINCT FROM $1) ORDER BY d.created_at, c.seq LIMIT $2`, model, limit-len(mem))
		if err != nil {
			return err
		}
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.seq, &r.text); err != nil {
				rows.Close()
				return err
			}
			chunks = append(chunks, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(mem)+len(chunks) == 0 {
			return nil
		}
		texts := make([]string, 0, len(mem)+len(chunks))
		for _, r := range mem {
			texts = append(texts, r.text)
		}
		for _, r := range chunks {
			texts = append(texts, r.text)
		}
		vecs, err := s.Embedder.Embed(ctx, texts)
		if err != nil {
			return err
		}
		b := &pgx.Batch{}
		for i, r := range mem {
			b.Queue(`UPDATE memory_records SET embedding = $2, embedding_model = $3 WHERE id = $1`, r.id, vecs[i], model)
		}
		for i, r := range chunks {
			b.Queue(`UPDATE knowledge_chunks SET embedding = $3, embedding_model = $4 WHERE document_id = $1 AND seq = $2`, r.id, r.seq, vecs[len(mem)+i], model)
		}
		if err := tx.SendBatch(ctx, b).Close(); err != nil {
			return err
		}
		n = len(mem) + len(chunks)
		return nil
	})
	return n, err
}
