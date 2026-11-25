package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/embedding"
)

func bigTable(minBytes int, rowFmt string, args func(i int) []any) string {
	var b strings.Builder
	b.WriteString("| code | rate |\n| --- | --- |\n")
	for i := 0; b.Len() < minBytes; i++ {
		fmt.Fprintf(&b, rowFmt, args(i)...)
	}
	return b.String()
}

// B3: a 40 KiB table is retrievable by one of its rows, and the excerpt shows that row (it used to show rows 0-81).
func TestLargeTableRowIsRetrievableAndExcerpted(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	text := "# Rates\n\n" + bigTable(40<<10, "| zq%06d | %d |\n", func(i int) []any { return []any{i, i} })
	doc := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://rates", "", text))
	if doc["chunkCount"].(float64) < 20 {
		t.Fatalf("chunks %v", doc["chunkCount"])
	}
	for _, row := range []string{"zq000000", "zq000123", "zq002000"} {
		got := items(resolveKnowledge(e, "carol", row, nil))
		if len(got) == 0 {
			t.Fatalf("%s: not retrievable", row)
		}
		ex := strings.TrimPrefix(got[0]["excerpt"].(string), "Rates\n") // the section line, then the rows
		if !strings.Contains(ex, row) || strings.Contains(ex, "…") || len(ex) > 1500 || !strings.HasPrefix(ex, "| code | rate |") {
			t.Fatalf("%s: excerpt %d bytes, has the row %v, head %q", row, len(ex), strings.Contains(ex, row), ex[:20])
		}
		if loc, _ := got[0]["locator"].(map[string]any); loc == nil || loc["headings"].([]any)[0] != "Rates" {
			t.Fatalf("%s: locator %v", row, got[0]["locator"])
		}
	}
}

// B3: a 3 MiB table (under MaxDocument) is accepted - it was a 500, the tsvector of its one chunk was over 1 MiB - and every chunk's index text is bounded.
func TestHugeTableIsAccepted(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	text := bigTable(3<<20, "| w%07dx%07d | v%07d |\n", func(i int) []any { return []any{i, i * 7, i * 13} })
	e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://big", "", text))
	if got := items(resolveKnowledge(e, "carol", "w0012345x0086415", nil)); len(got) == 0 {
		t.Fatal("a row of the huge table is not retrievable")
	}
	conn, err := pgx.Connect(context.Background(), e.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var max, n int
	if err := conn.QueryRow(context.Background(), `SELECT max(octet_length(index_text)), count(*) FROM knowledge_chunks`).Scan(&max, &n); err != nil || max > 8192 || n < 1000 {
		t.Fatalf("longest index text %d bytes in %d chunks (%v)", max, n, err)
	}
}

// B3, CJK: a table of Chinese text and a long Chinese row are cut on characters and found by their bigrams.
func TestCJKTableRows(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	text := "# 运价表\n\n" + bigTable(20<<10, "| 线路 zx%05d | 每公斤十二元 |\n", func(i int) []any { return []any{i} }) + "\n| 备注 | " + strings.Repeat("运费按重量计算，每公斤十二元。", 200) + " |\n"
	e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://cjk", "", text))
	got := items(resolveKnowledge(e, "carol", "zx00300", nil))
	if len(got) == 0 || !strings.Contains(got[0]["excerpt"].(string), "线路 zx00300") {
		t.Fatalf("a CJK table row: %v", got)
	}
	for _, it := range items(resolveKnowledge(e, "carol", "运费按重量计算", nil)) {
		if ex := it["excerpt"].(string); strings.ContainsRune(ex, '�') {
			t.Fatalf("a character was split: %q", ex)
		}
	}
}

// poison refuses any text that contains the word and embeds the rest.
type poison struct {
	*embedding.Fake
	calls atomic.Int32
}

func (p *poison) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	p.calls.Add(1)
	for _, t := range texts {
		if strings.Contains(t, "poisonword") {
			return nil, &embedding.StatusError{Status: 413}
		}
	}
	return p.Fake.Embed(ctx, texts)
}

// B3: a chunk the embedder cannot take is skipped with its reason and never stalls the backfill or the projector; an
// endpoint that is down is not mistaken for a poisoned chunk.
func TestBackfillSkipsAChunkThatCannotBeEmbedded(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	p := &poison{Fake: &embedding.Fake{Dims: 64}}
	e.store.Embedder = nil // written while no embedder is configured: the rows wait for the backfill
	text := "# A\n\nfirst fine paragraph " + strings.Repeat("alpha ", 5) + "\n\n" + strings.Repeat("filler words here ", 90) + "\n\nthe poisonword paragraph\n\n" + strings.Repeat("more filler text ", 90) + "\n\nlast fine paragraph tail"
	doc := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://poison", "", text))
	if doc["chunkCount"].(float64) < 3 {
		t.Fatalf("chunks %v", doc["chunkCount"])
	}
	e.store.Embedder = p
	ctx := context.Background()
	// the endpoint is down: nothing is skipped
	down := &downEmbedder{}
	e.store.Embedder = down
	if _, err := e.store.BackfillEmbeddings(ctx, 100); err == nil {
		t.Fatal("a dead endpoint is not an error")
	}
	var skipped int
	conn, err := pgx.Connect(ctx, e.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	_ = conn.QueryRow(ctx, `SELECT count(*) FROM knowledge_chunks WHERE embed_error IS NOT NULL`).Scan(&skipped)
	if skipped != 0 {
		t.Fatalf("%d chunks skipped because the endpoint was down", skipped)
	}
	e.store.Embedder = p
	for i := 0; i < 3; i++ { // several runs: it does not stall and it does not keep retrying the skipped chunk
		if _, err := e.store.BackfillEmbeddings(ctx, 100); err != nil {
			t.Fatal(err)
		}
	}
	var reason string
	var embedded, total int
	_ = conn.QueryRow(ctx, `SELECT count(*) FILTER (WHERE embedding IS NOT NULL), count(*), coalesce(max(embed_error), '') FROM knowledge_chunks`).Scan(&embedded, &total, &reason)
	if embedded != total-1 || !strings.Contains(reason, "413") {
		t.Fatalf("%d of %d chunks embedded; recorded reason %q", embedded, total, reason)
	}
	calls := p.calls.Load()
	if _, err := e.store.BackfillEmbeddings(ctx, 100); err != nil || p.calls.Load() != calls {
		t.Fatalf("the skipped chunk is tried again (%d calls, %v)", p.calls.Load()-calls, err)
	}
	// the document is retrievable lexically, the poisoned chunk included
	if got := items(resolveKnowledge(e, "carol", "poisonword", nil)); len(got) == 0 {
		t.Fatal("a chunk skipped for its vector is gone from the lexical index")
	}

	// the projector: one poisoned event among others
	e2 := newEnv(t)
	kbSources(e2)
	e2.store.Embedder = nil
	e2.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://poison2", "", text))
	e2.store.Embedder = &poison{Fake: &embedding.Fake{Dims: 64}}
	if n, err := e2.store.Project(ctx, 100); err != nil || n == 0 {
		t.Fatalf("projection with a poisoned chunk: %d %v", n, err)
	}
	st := e2.must(200, "GET", "/api/v1/projections", "alice", nil)
	if st["pending"].(float64) != 0 {
		t.Fatalf("events pending after the run: %v", st)
	}
}

type downEmbedder struct{}

func (downEmbedder) Model() string { return "down" }
func (downEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("connection refused")
}
