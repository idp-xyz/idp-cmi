package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"idp-cmi/adapters/store/postgres"
)

// pgvector (ADR-0001 D38): the native path must rank like the SQL function, follow erasure, and an HNSW
// index must be usable by the query. Skipped on a PostgreSQL without the extension (make test-integration-pgvector).
func TestPgvectorPath(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	on, _ := e.store.Vectors(ctx)
	if !on {
		t.Skip("pgvector is not installed in this PostgreSQL (make test-integration-pgvector)")
	}
	e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "assistant", "displayName": "Assistant", "kind": "agent", "trust": "informational"})
	contents := []string{"Morning pickup slots run from 07:00 to 11:00", "Customs duties are prepaid when the sender chooses DDP", "Returns are accepted within 30 days of delivery",
		"Fragile parcels need five centimetres of cushioning", "Saturday delivery is available in selected postcodes", "Tracking events refresh every fifteen minutes"}
	for i, c := range contents {
		e.must(201, "POST", "/v1/memory/records", "bob", memory("assistant", fmt.Sprintf("v%d", i), "tenant", "", c, "internal", e.now))
	}
	ask := func(q string) []string {
		var out []string
		for _, it := range items(e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": q, "purpose": "test", "budget": map[string]any{"maxItems": 6}})) {
			out = append(out, fmt.Sprintf("%v@%.3f", it["excerpt"], it["score"]))
		}
		return out
	}
	queries := []string{"collection slots in the morning", "who pays customs duties", "how long to send something back", "weekend delivery"}
	exact := map[string][]string{}
	e.store.DisableVectors(true)
	for _, q := range queries {
		exact[q] = ask(q)
	}
	e.store.DisableVectors(false)
	if on, _ := e.store.Vectors(ctx); !on {
		t.Fatal("vectors did not come back")
	}
	for _, q := range queries {
		if got := ask(q); strings.Join(got, "|") != strings.Join(exact[q], "|") {
			t.Errorf("pgvector exact scan differs from the SQL cosine for %q:\n  sql      %v\n  pgvector %v", q, exact[q], got)
		}
	}

	t.Run("copy-follows-erasure", func(t *testing.T) {
		var drift int
		err := e.store.InTenant(ctx, e.tenantA, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE memory_records SET embedding = NULL, embedding_model = 'erased' WHERE content LIKE 'Customs%'`); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM memory_records WHERE (embedding IS NULL) <> (embedding_v IS NULL)`).Scan(&drift)
		})
		if err != nil || drift != 0 {
			t.Fatalf("the vector copy must be cleared with the array: drift %d, %v", drift, err)
		}
	})

	t.Run("hnsw-index", func(t *testing.T) {
		if err := postgres.CreateVectorIndex(ctx, e.ownerURL, 64, "64MB", 8, 32); err != nil {
			t.Fatal(err)
		}
		if err := postgres.CreateVectorIndex(ctx, e.ownerURL, 64, "64MB", 8, 32); err != nil { // idempotent
			t.Fatal(err)
		}
		if err := postgres.CreateVectorIndex(ctx, e.ownerURL, 64, "lots", 0, 0); err == nil {
			t.Fatal("a malformed memory size must be refused, it goes into a statement")
		}
		e.store.DisableVectors(false)
		_, idx := e.store.Vectors(ctx)
		if len(idx["memory_records"]) != 1 || idx["memory_records"][0] != 64 || len(idx["knowledge_chunks"]) != 1 {
			t.Fatalf("indexes not detected: %v", idx)
		}
		// the query in the shape the store emits can use the partial expression index
		conn, err := pgx.Connect(ctx, e.ownerURL)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		var plan []string
		_, _ = conn.Exec(ctx, "SET enable_seqscan = off")
		rows, err := conn.Query(ctx, `EXPLAIN SELECT id FROM memory_records m WHERE m.embedding IS NOT NULL AND vector_dims(m.embedding_v) = 64
			ORDER BY (m.embedding_v::vector(64)) <=> ($1::real[]::vector(64)) ASC LIMIT 5`, make([]float32, 64))
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var l string
			_ = rows.Scan(&l)
			plan = append(plan, l)
		}
		rows.Close()
		if !strings.Contains(strings.Join(plan, "\n"), "memory_records_hnsw_64") {
			t.Fatalf("the HNSW index is not usable by the store's query shape:\n%s", strings.Join(plan, "\n"))
		}
		// and the answers through the index agree with the exact ones
		for _, q := range []string{"collection slots in the morning", "weekend delivery"} {
			if got := ask(q); len(got) == 0 || strings.Split(got[0], "@")[0] != strings.Split(exact[q][0], "@")[0] {
				t.Errorf("through the index %q: %v, exact %v", q, got, exact[q])
			}
		}
	})
}
