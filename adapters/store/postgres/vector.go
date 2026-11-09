package postgres

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// pgvector support (ADR-0001 D38). The canonical vector stays the real[] column (no extension needed);
// when the vector extension is installed, Migrate also keeps a `vector` copy (embedding_v) in step through
// a trigger, and the semantic retriever reads that copy with the native distance operator - exact scans
// in C instead of a SQL function per row, and an HNSW index per vector size once the table is large.
// Everything works without the extension: Store.vectors is then false and the old function is used.

const vectorSetupSQL = `
DO $setup$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector') THEN
    RAISE NOTICE 'pgvector is not available: semantic retrieval uses the SQL cosine function';
    RETURN;
  END IF;
  BEGIN
    CREATE EXTENSION IF NOT EXISTS vector;
  EXCEPTION WHEN insufficient_privilege THEN
    RAISE NOTICE 'pgvector is available but this role may not create it: semantic retrieval uses the SQL cosine function';
    RETURN;
  END;
  ALTER TABLE memory_records ADD COLUMN IF NOT EXISTS embedding_v vector;
  ALTER TABLE knowledge_chunks ADD COLUMN IF NOT EXISTS embedding_v vector;
  CREATE OR REPLACE FUNCTION sync_embedding_v() RETURNS trigger LANGUAGE plpgsql AS $f$
  BEGIN
    IF NEW.embedding IS NULL OR cardinality(NEW.embedding) = 0 THEN
      NEW.embedding_v := NULL;
    ELSE
      NEW.embedding_v := NEW.embedding::vector;
    END IF;
    RETURN NEW;
  END $f$;
  DROP TRIGGER IF EXISTS memory_records_embedding_v ON memory_records;
  CREATE TRIGGER memory_records_embedding_v BEFORE INSERT OR UPDATE OF embedding ON memory_records FOR EACH ROW EXECUTE FUNCTION sync_embedding_v();
  DROP TRIGGER IF EXISTS knowledge_chunks_embedding_v ON knowledge_chunks;
  CREATE TRIGGER knowledge_chunks_embedding_v BEFORE INSERT OR UPDATE OF embedding ON knowledge_chunks FOR EACH ROW EXECUTE FUNCTION sync_embedding_v();
  -- rows embedded before the extension was there
  UPDATE memory_records SET embedding_v = embedding::vector WHERE embedding IS NOT NULL AND embedding_v IS NULL AND cardinality(embedding) > 0;
  UPDATE knowledge_chunks SET embedding_v = embedding::vector WHERE embedding IS NOT NULL AND embedding_v IS NULL AND cardinality(embedding) > 0;
END
$setup$;`

// EnsureVectors installs the pgvector copy when the extension is available; idempotent, a no-op otherwise.
func ensureVectors(ctx context.Context, conn *pgx.Conn) error {
	_, err := conn.Exec(ctx, vectorSetupSQL)
	return err
}

// vectorState is what the service learned about pgvector: whether the copy column exists and which
// vector sizes have an HNSW index on each table. Refreshed lazily.
type vectorState struct {
	mu       sync.Mutex
	loaded   time.Time
	enabled  bool
	hnsw     map[string]map[int]bool // table -> dims
	disabled bool                    // forced off (tests, or an operator who wants the SQL function)
}

var hnswName = regexp.MustCompile(`^(memory_records|knowledge_chunks)_hnsw_([0-9]+)$`)

const vectorStateTTL = time.Minute

func (s *Store) vectorInfo(ctx context.Context) (enabled bool, hnsw map[string]map[int]bool) {
	st := &s.vec
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.disabled {
		return false, nil
	}
	if time.Since(st.loaded) < vectorStateTTL && st.hnsw != nil {
		return st.enabled, st.hnsw
	}
	st.hnsw = map[string]map[int]bool{}
	st.loaded = time.Now()
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_extension e WHERE e.extname = 'vector'
		AND EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'memory_records' AND column_name = 'embedding_v')
		AND EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'knowledge_chunks' AND column_name = 'embedding_v')`).Scan(&n); err != nil || n == 0 {
		st.enabled = false
		return false, st.hnsw
	}
	st.enabled = true
	rows, err := s.Pool.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid WHERE i.indisvalid AND i.indisready AND c.relname ~ '_hnsw_[0-9]+$'`)
	if err != nil {
		return true, st.hnsw
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			if m := hnswName.FindStringSubmatch(name); m != nil {
				d, _ := strconv.Atoi(m[2])
				if st.hnsw[m[1]] == nil {
					st.hnsw[m[1]] = map[int]bool{}
				}
				st.hnsw[m[1]][d] = true
			}
		}
	}
	return true, st.hnsw
}

// DisableVectors forces the SQL cosine function (tests that compare the two paths).
func (s *Store) DisableVectors(off bool) {
	s.vec.mu.Lock()
	s.vec.disabled = off
	s.vec.loaded = time.Time{}
	s.vec.hnsw = nil
	s.vec.mu.Unlock()
}

// Vectors reports whether the pgvector path is in use, and the vector sizes with an HNSW index on a table.
func (s *Store) Vectors(ctx context.Context) (enabled bool, indexed map[string][]int) {
	on, h := s.vectorInfo(ctx)
	out := map[string][]int{}
	for t, ds := range h {
		for d := range ds {
			out[t] = append(out[t], d)
		}
	}
	return on, out
}

// semanticSQL returns the three pieces of a semantic query on table (alias a, vector column embedding,
// the query vector is $4): the score expression (cosine similarity), an extra filter, and the ORDER BY.
// ef is the number of candidates an HNSW scan should keep (0 = no index in play).
func (s *Store) semanticSQL(ctx context.Context, table, a string, dims int) (score, filter, order string, ef int) {
	enabled, hnsw := s.vectorInfo(ctx)
	if !enabled || dims <= 0 || dims > 16000 {
		return "cosine_similarity(" + a + ".embedding, $4::real[])::float8", "", "r DESC NULLS LAST", 0
	}
	filter = fmt.Sprintf(" AND vector_dims(%s.embedding_v) = %d", a, dims)
	if hnsw[table][dims] {
		dist := fmt.Sprintf("(%s.embedding_v::vector(%d)) <=> ($4::real[]::vector(%d))", a, dims, dims)
		return "(1 - (" + dist + "))::float8", filter, dist + " ASC", 1
	}
	return "(1 - (" + a + ".embedding_v <=> $4::real[]::vector))::float8", filter, "r DESC NULLS LAST", 0
}

// tuneHNSW sets the per-transaction scan options an HNSW query needs: enough candidates for the LIMIT
// (the default of 40 would silently cap a LIMIT 200 at 40 rows) and iterative scans, so a selective
// visibility filter keeps walking the graph instead of returning too few rows.
func tuneHNSW(ctx context.Context, tx pgx.Tx, limit int) error {
	ef := limit
	if ef < 40 {
		ef = 40
	}
	if ef > 1000 {
		ef = 1000
	}
	_, err := tx.Exec(ctx, `SELECT set_config('hnsw.ef_search', $1, true), set_config('hnsw.iterative_scan', 'relaxed_order', true), set_config('hnsw.max_scan_tuples', '40000', true)`, strconv.Itoa(ef))
	return err
}

// CreateVectorIndex builds the HNSW indexes for vectors of the given size on both tables, concurrently
// (no write lock), as the schema owner. maintenanceMem is a PostgreSQL size such as "2GB": index builds
// are an order of magnitude slower when the graph does not fit in it.
func CreateVectorIndex(ctx context.Context, ownerURL string, dims int, maintenanceMem string, m, efConstruction int) error {
	if dims < 1 || dims > 2000 {
		return fmt.Errorf("dims must be within [1, 2000] (pgvector's HNSW limit for vector)")
	}
	if !regexp.MustCompile(`^[0-9]+(kB|MB|GB)$`).MatchString(maintenanceMem) {
		return fmt.Errorf("maintenance memory %q is not a PostgreSQL size such as 1GB", maintenanceMem)
	}
	if m == 0 {
		m = 16
	}
	if efConstruction == 0 {
		efConstruction = 64
	}
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SET maintenance_work_mem = '"+maintenanceMem+"'"); err != nil {
		return err
	}
	for _, t := range []string{"memory_records", "knowledge_chunks"} {
		stmt := fmt.Sprintf(`CREATE INDEX CONCURRENTLY IF NOT EXISTS %s_hnsw_%d ON %s USING hnsw ((embedding_v::vector(%d)) vector_cosine_ops) WITH (m = %d, ef_construction = %d) WHERE vector_dims(embedding_v) = %d`,
			t, dims, t, dims, m, efConstruction, dims)
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", strings.Fields(stmt)[5], err)
		}
	}
	return nil
}
