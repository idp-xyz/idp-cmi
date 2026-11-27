package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
	"idp-cmi/internal/embedding"
)

// shortReason bounds a recorded failure reason.
func shortReason(s string) string {
	if s == "" {
		s = "the embedder refused the text"
	}
	return cutUTF8(s, 300)
}

func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}

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
			id      string
			seq     int
			text    string
			version int // memory rows: the version the text was read at
		}
		var mem, chunks []row
		ceiling := sensitivityRankSQL("coalesce(tp.external_model_ceiling, 'internal')")
		rows, err := tx.Query(ctx, `SELECT m.id::text, m.content, m.version FROM memory_records m LEFT JOIN tenant_policies tp ON tp.tenant_id = m.tenant_id
			WHERE m.status = 'active' AND m.erasure_id IS NULL AND (m.embedding IS NULL OR m.embedding_model IS DISTINCT FROM $1) AND `+sensitivityRankSQL("m.sensitivity")+` <= `+ceiling+` ORDER BY m.created_at LIMIT $2`, model, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.text, &r.version); err != nil {
				rows.Close()
				return err
			}
			mem = append(mem, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT c.document_id::text, c.seq, c.index_text FROM knowledge_chunks c JOIN knowledge_documents d ON d.id = c.document_id LEFT JOIN tenant_policies tp ON tp.tenant_id = d.tenant_id
			WHERE d.status IN ('active', 'superseded') AND d.erasure_id IS NULL AND (c.embed_error IS NULL OR c.embed_error_model IS DISTINCT FROM $1) AND (c.embed_retry_at IS NULL OR c.embed_retry_at <= now()) AND (c.embedding IS NULL OR c.embedding_model IS DISTINCT FROM $1) AND `+sensitivityRankSQL("d.sensitivity")+` <= `+ceiling+` ORDER BY d.created_at, c.seq LIMIT $2`, model, limit-len(mem))
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
		b := &pgx.Batch{}
		if len(mem) > 0 {
			texts := make([]string, len(mem))
			for i, r := range mem {
				texts[i] = r.text
			}
			vecs, err := s.Embedder.Embed(ctx, texts)
			if err != nil {
				return err
			}
			for i, r := range mem {
				// guarded by the version read and by the erasure barrier: the projection runs beside this loop (D79) and a row that moved on
				// while the embedder worked is the projection's, not a stale vector's
				b.Queue(`UPDATE memory_records SET embedding = $2, embedding_model = $3 WHERE id = $1 AND version = $4 AND erasure_id IS NULL`, r.id, vecs[i], model, r.version)
			}
		}
		if len(chunks) > 0 { // a chunk the embedder cannot take is skipped with its reason, and the others go on
			texts := make([]string, len(chunks))
			for i, r := range chunks {
				texts[i] = cutUTF8(r.text, cmi.MaxIndexTextBytes) // rows written before the bound existed
			}
			vecs, fails, err := s.embedEach(ctx, texts)
			if err != nil {
				return err
			}
			for i, r := range chunks {
				s.queueChunkEmbedding(b, r.id, r.seq, vecs[i], fails[i], model)
			}
		}
		if err := tx.SendBatch(ctx, b).Close(); err != nil {
			return err
		}
		n = len(mem) + len(chunks)
		return nil
	})
	return n, err
}

// queueChunkEmbedding queues what an embedding attempt of one chunk leaves: the vector (clearing any earlier failure), a
// permanent failure recorded with the model it was for (the chunk is skipped until the model changes or an administrator
// clears it), or a transient failure that counts an attempt and sets the time of the next one (10 s, doubling, at most an hour).
func (s *Store) queueChunkEmbedding(b *pgx.Batch, doc string, seq int, vec []float32, f embedFail, model string) {
	switch {
	case vec != nil:
		b.Queue(`UPDATE knowledge_chunks SET embedding = $3, embedding_model = $4, embed_error = NULL, embed_error_model = NULL, embed_attempts = 0, embed_retry_at = NULL WHERE document_id = $1 AND seq = $2`, doc, seq, vec, model)
	case f.Permanent:
		if s.OnEmbedFailure != nil {
			s.OnEmbedFailure("permanent")
		}
		b.Queue(`UPDATE knowledge_chunks SET embed_error = $3, embed_error_model = $4 WHERE document_id = $1 AND seq = $2`, doc, seq, shortReason(f.Reason), model)
		if s.Log != nil {
			s.Log.Warn("a chunk cannot be embedded and is skipped", "document", doc, "seq", seq, "reason", shortReason(f.Reason))
		}
	default:
		kind := "transient"
		if embedding.Refused(f.Err) {
			kind = "refused" // 401, 403, 404...: the embedder's credential or address, not the text
			if s.Log != nil {
				s.Log.Error("the embedding endpoint is refusing requests (check its credential and address); chunks wait and are retried", "document", doc, "error", shortReason(f.Reason))
			}
		}
		if s.OnEmbedFailure != nil {
			s.OnEmbedFailure(kind)
		}
		b.Queue(`UPDATE knowledge_chunks SET embed_attempts = embed_attempts + 1, embed_retry_at = now() + least(interval '1 hour', interval '10 seconds' * power(2, least(embed_attempts, 12))) WHERE document_id = $1 AND seq = $2`, doc, seq)
		if s.Log != nil {
			s.Log.Warn("a chunk could not be embedded this time; it will be tried again", "document", doc, "seq", seq, "reason", shortReason(f.Reason))
		}
	}
}

// ClearEmbedErrors makes the chunks that were skipped for a permanent embedding error, or are waiting out a retry, eligible
// again at once (an administrator's way to retry after fixing the embedder or its input limit). It returns how many.
func (s *Store) ClearEmbedErrors(ctx context.Context, tenant, by string) (int, error) {
	n := 0
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE knowledge_chunks SET embed_error = NULL, embed_error_model = NULL, embed_attempts = 0, embed_retry_at = NULL WHERE embed_error IS NOT NULL OR embed_retry_at IS NOT NULL`)
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		return Audit(ctx, tx, tenant, by, "knowledge.embed_errors_cleared", "", map[string]any{"chunks": n})
	})
	return n, err
}
