package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/config"
	"idp-cmi/internal/embedding"
	"idp-cmi/internal/embedwire"
	"idp-cmi/internal/observability"
)

// ADR-0001 D59: queries have an embedding endpoint of their own. These tests stand two test embedders in for the two
// endpoints and watch which text each of them is given.

// endpoint is a test embedder that remembers every text it was asked for (the probe text excepted).
type endpoint struct {
	base embedding.Embedder
	mu   sync.Mutex
	seen []string
	err  error // when set, every call fails with it
	n    int   // calls, the probe's included
}

func (p *endpoint) Model() string { return p.base.Model() }
func (p *endpoint) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	p.mu.Lock()
	p.n++
	for _, t := range texts {
		if t != embedding.ProbeText {
			p.seen = append(p.seen, t)
		}
	}
	err := p.err
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return p.base.Embed(ctx, texts)
}
func (p *endpoint) saw(word string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Contains(strings.Join(p.seen, "\n"), word)
}
func (p *endpoint) calls() int       { p.mu.Lock(); defer p.mu.Unlock(); return p.n }
func (p *endpoint) setErr(err error) { p.mu.Lock(); p.err = err; p.mu.Unlock() }

// reversed answers with every vector back to front: the same dimension, a different model.
type reversed struct{ embedding.Embedder }

func (r reversed) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vs, err := r.Embedder.Embed(ctx, texts)
	for _, v := range vs {
		for i, j := 0, len(v)-1; i < j; i, j = i+1, j-1 {
			v[i], v[j] = v[j], v[i]
		}
	}
	return vs, err
}

// wireCfg is the embedding configuration of the tests: prefixes made of stop words, which the test embedder ignores, so that the
// vectors do not change and the texts that reach each endpoint can still be told apart.
func wireCfg() *config.Embedding {
	return &config.Embedding{BaseURL: "http://main/v1", Model: "m", QueryPrefix: "the ", DocumentPrefix: "an "}
}

// queryEnv wires the environment with embedwire.Build, the code main.go uses: documents on one endpoint, queries behind a Guard
// on the other. The synonym "collection" = "pickup" lets only the semantic side find the record by that word.
func queryEnv(t *testing.T, queryEP embedding.Embedder) (e *env, docs *endpoint, query *endpoint, g *embedding.Guard) {
	t.Helper()
	e = newEnv(t)
	docs = &endpoint{base: e.embedder}
	query = &endpoint{base: queryEP}
	w := embedwire.Build(embedwire.Options{Config: wireCfg(), Main: docs, Query: query, Store: e.store, Metrics: e.host.Metrics})
	e.store.Embedder = w.Documents
	e.host.Resolver.Embedder = w.Queries
	g = w.Guard
	e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "assistant", "displayName": "Assistant", "kind": "agent", "trust": "informational"})
	return
}

func metricsText(e *env) string {
	var b strings.Builder
	e.host.Metrics.Write(&b)
	return b.String()
}

func writeSlots(e *env) {
	e.must(201, "POST", "/v1/memory/records", "bob", memory("assistant", "slots", "tenant", "", "Morning pickup slots run from 07:00 to 11:00", "internal", e.now))
}

func resolveText(e *env, q string) []map[string]any {
	return items(e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": q, "purpose": "test", "budget": map[string]any{"maxItems": 6}}))
}

func found(its []map[string]any, word string) bool { return strings.Contains(fmt.Sprint(its), word) }

func TestQueriesAndDocumentsUseTheirOwnEndpoints(t *testing.T) {
	e, docs, query, g := queryEnv(t, &embedding.Fake{Dims: 64, Synonyms: map[string]string{"collection": "pickup", "collections": "pickup"}})
	if st := g.Check(context.Background()); st != embedding.StateOK {
		t.Fatalf("probe: %s", st)
	}
	writeSlots(e) // a memory write is a document
	e.store.Embedder = nil
	e.must(201, "POST", "/v1/memory/records", "bob", memory("assistant", "late", "tenant", "", "Backfilled latecomer sentence about duties", "internal", e.now))
	e.store.Embedder = docs // the backfill is a document too
	if n, err := e.store.BackfillEmbeddings(context.Background(), 100); err != nil || n < 1 {
		t.Fatalf("backfill %d %v", n, err)
	}
	// the word "collection" is only the record's pickup to the embedder: found by meaning, and the query went to its endpoint
	if !found(resolveText(e, "collection slots"), "pickup slots") {
		t.Fatal("the semantic retriever found nothing on the query endpoint")
	}
	if !docs.saw("pickup slots") || !docs.saw("latecomer") || docs.saw("collection slots") {
		t.Fatalf("documents endpoint saw %q", docs.seen)
	}
	if !query.saw("collection slots") || query.saw("pickup slots") || query.saw("latecomer") {
		t.Fatalf("query endpoint saw %q", query.seen)
	}
	// each side with its own prefix, and counted by role
	if !query.saw("the collection slots") || !docs.saw("an Morning pickup slots") || docs.saw("the collection") {
		t.Fatalf("prefixes: query %q, documents %q", query.seen, docs.seen)
	}
	m := metricsText(e)
	for _, want := range []string{`idp_cmi_embedding_requests_total{role="query",outcome="ok"} 1`, `idp_cmi_query_endpoint_state{state="ok"} 1`, `idp_cmi_query_endpoint_probes_total{result="ok"} 1`} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	if !strings.Contains(m, `idp_cmi_embedding_requests_total{role="documents",outcome="ok"}`) {
		t.Error("documents requests are not counted")
	}
}

// A query endpoint that does not serve the same model is not used: not with another dimension, not with another model.
func TestAMismatchedQueryEndpointIsNotUsed(t *testing.T) {
	for name, ep := range map[string]embedding.Embedder{
		"dimension": &embedding.Fake{Dims: 32, Synonyms: map[string]string{"collection": "pickup"}},
		"model":     reversed{&embedding.Fake{Dims: 64, Synonyms: map[string]string{"collection": "pickup"}}},
	} {
		t.Run(name, func(t *testing.T) {
			e, docs, query, g := queryEnv(t, ep)
			writeSlots(e)
			if st := g.Check(context.Background()); st != embedding.StateMismatch {
				t.Fatalf("probe: %s", st)
			}
			docs.mu.Lock()
			docs.seen = nil
			docs.mu.Unlock()
			// lexical only: "collection" is not in the record, "morning" is
			if found(resolveText(e, "collection"), "pickup slots") {
				t.Fatal("found by meaning through an endpoint that failed its probe")
			}
			if !found(resolveText(e, "morning"), "pickup slots") {
				t.Fatal("the lexical answer is gone")
			}
			if query.saw("collection") || query.saw("morning") {
				t.Fatalf("a query reached the mismatched endpoint: %q", query.seen)
			}
			if docs.saw("collection") || docs.saw("morning") {
				t.Fatalf("a query fell back to the documents endpoint: %q", docs.seen)
			}
		})
	}
}

// Before the first probe has finished the query endpoint is not used either.
func TestAQueryEndpointNotYetCheckedIsNotUsed(t *testing.T) {
	e, docs, query, g := queryEnv(t, &embedding.Fake{Dims: 64, Synonyms: map[string]string{"collection": "pickup"}})
	writeSlots(e)
	if g.State() != embedding.StatePending || found(resolveText(e, "collection"), "pickup slots") || query.saw("collection") || docs.saw("collection") {
		t.Fatalf("state %s, query saw %q, documents saw %q", g.State(), query.seen, docs.seen)
	}
}

// The query endpoint goes down after it passed: the resolve answers from words, 200, and nothing is marked permanent
// (D58 is about documents, even when the endpoint says 413 for a query).
func TestResolveAnswersLexicallyWhenTheQueryEndpointIsDown(t *testing.T) {
	e, docs, query, g := queryEnv(t, &embedding.Fake{Dims: 64, Synonyms: map[string]string{"collection": "pickup"}})
	writeSlots(e)
	e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "handbook", "displayName": "Handbook", "kind": "document-store", "trust": "informational"})
	e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://slots", "", "# Slots\n\nMorning pickup slots for parcels"))
	if g.Check(context.Background()) != embedding.StateOK {
		t.Fatal("probe")
	}
	for _, err := range []error{errors.New("connection refused"), &embedding.StatusError{Status: 413}, &embedding.StatusError{Status: 503}} {
		query.setErr(err)
		docs.mu.Lock()
		docs.seen = nil
		docs.mu.Unlock()
		its := resolveText(e, "morning slots")
		if !found(its, "pickup slots") {
			t.Fatalf("%v: no lexical answer: %v", err, its)
		}
		if found(resolveText(e, "collection"), "pickup slots") {
			t.Fatalf("%v: found by meaning without a query vector", err)
		}
		if docs.saw("morning slots") || docs.saw("collection") {
			t.Fatalf("%v: a query went to the documents endpoint", err)
		}
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, e.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var marked int
	_ = conn.QueryRow(ctx, `SELECT count(*) FROM knowledge_chunks WHERE embed_error IS NOT NULL OR embed_attempts > 0`).Scan(&marked)
	if marked != 0 {
		t.Fatalf("%d chunks marked by a query endpoint failure", marked)
	}
	query.setErr(nil)
	if !found(resolveText(e, "collection"), "pickup slots") {
		t.Fatal("the endpoint is back and queries do not use it")
	}
}

// Without embedding.query nothing changes: the query is embedded, with its prefix, by the one endpoint that embeds documents.
func TestWithoutAQueryEndpointQueriesUseTheMainOne(t *testing.T) {
	e := newEnv(t)
	main := &endpoint{base: e.embedder}
	w := embedwire.Build(embedwire.Options{Config: wireCfg(), Main: main, Store: e.store, Metrics: e.host.Metrics})
	if w.Guard != nil {
		t.Fatal("a probe without a query endpoint")
	}
	e.store.Embedder = w.Documents
	e.host.Resolver.Embedder = w.Queries
	e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "assistant", "displayName": "Assistant", "kind": "agent", "trust": "informational"})
	writeSlots(e)
	if !found(resolveText(e, "collection slots"), "pickup slots") {
		t.Fatal("the semantic retriever found nothing")
	}
	if !main.saw("the collection slots") || !main.saw("an Morning pickup slots") {
		t.Fatalf("the main endpoint saw %q", main.seen)
	}
	if !strings.Contains(metricsText(e), `idp_cmi_embedding_requests_total{role="query",outcome="ok"} 1`) || strings.Contains(metricsText(e), "idp_cmi_query_endpoint_state") {
		t.Fatalf("metrics:\n%s", metricsText(e))
	}
}

// D59: the probe's reference vector is kept in the database under the model identity, so a restart during a bulk ingest does
// not leave queries lexical until the documents endpoint answers.
func TestTheProbeReferenceSurvivesARestart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	fake := &embedding.Fake{Dims: 64}
	build := func(main, query *endpoint, cfg *config.Embedding) *embedwire.Result {
		return embedwire.Build(embedwire.Options{Config: cfg, Main: main, Query: query, Store: e.store, Metrics: observability.NewMetrics()})
	}
	// no reference and a documents endpoint that is busy: pending, and it is tried again (not unreachable for ever)
	busy := &endpoint{base: fake, err: errors.New("queued behind an ingest")}
	query := &endpoint{base: fake}
	w := build(busy, query, wireCfg())
	for i := 0; i < 2; i++ {
		if st := w.Guard.Check(ctx); st != embedding.StatePending {
			t.Fatalf("no reference, documents endpoint busy: %s", st)
		}
	}
	// the documents endpoint answers once: the answer is kept
	busy.setErr(nil)
	if st := w.Guard.Check(ctx); st != embedding.StateOK {
		t.Fatalf("%s", st)
	}
	// restart: a new process, the documents endpoint never answers; the stored reference is what the query endpoint is checked against
	never := &endpoint{base: fake, err: errors.New("queued behind an ingest")}
	w2 := build(never, query, wireCfg())
	if st := w2.Guard.Check(ctx); st != embedding.StateOK || never.calls() != 0 {
		t.Fatalf("after the restart: %s, the documents endpoint was asked %d times", st, never.calls())
	}
	// a query endpoint that is not that model is still caught by the stored reference
	other := &endpoint{base: reversed{fake}}
	if st := build(never, other, wireCfg()).Guard.Check(ctx); st != embedding.StateMismatch {
		t.Fatalf("a different model against the stored reference: %s", st)
	}
	// another model identity (the document prefix changed) has no reference: it is not borrowed
	cfg := wireCfg()
	cfg.DocumentPrefix = "a "
	if st := build(never, query, cfg).Guard.Check(ctx); st != embedding.StatePending {
		t.Fatalf("a reference of another model identity was used: %s", st)
	}
	m1, m2 := build(never, query, wireCfg()).Documents.Model(), build(never, query, cfg).Documents.Model()
	if m1 == m2 {
		t.Fatal("the test needs two model identities")
	}
	if v, err := e.store.ProbeReference(ctx, m2, embedding.ProbeText); err != nil || v != nil {
		t.Fatalf("reference of %s: %v %v", m2, v, err)
	}
	if v, err := e.store.ProbeReference(ctx, m1, embedding.ProbeText); err != nil || len(v) != 64 {
		t.Fatalf("reference of %s: %d %v", m1, len(v), err)
	}
}

// An embedder outage is counted: once per failed request and per role, without spending any chunk's attempts, and the
// last-success gauge of the role stays where it was.
func TestAnEmbedderOutageIsCountedPerRole(t *testing.T) {
	e, docs, query, g := queryEnv(t, &embedding.Fake{Dims: 64, Synonyms: map[string]string{"collection": "pickup"}})
	ctx := context.Background()
	g.Check(ctx)
	before := metricsText(e)
	if !strings.Contains(before, `idp_cmi_embedding_last_success_timestamp_seconds{role="documents"} 0`) {
		t.Fatalf("a success before any request:\n%s", before)
	}
	// a record written while the documents endpoint is down waits for the backfill
	docs.setErr(errors.New("connection refused"))
	writeSlots(e) // the write path asks once and is left for the backfill
	if _, err := e.store.BackfillEmbeddings(ctx, 100); err == nil {
		t.Fatal("a dead endpoint is not an error")
	}
	m := metricsText(e)
	if !strings.Contains(m, `idp_cmi_embed_failures_total{kind="unavailable",role="documents"} 2`) { // the write and the backfill
		t.Fatalf("documents outage not counted once per request:\n%s", m)
	}
	conn, err := pgx.Connect(ctx, e.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var spent int
	_ = conn.QueryRow(ctx, `SELECT count(*) FROM knowledge_chunks WHERE embed_attempts > 0 OR embed_error IS NOT NULL`).Scan(&spent)
	if spent != 0 {
		t.Fatalf("%d chunks had attempts spent on an outage", spent)
	}
	// the query side, once per resolve
	query.setErr(errors.New("connection refused"))
	resolveText(e, "morning")
	resolveText(e, "morning")
	m = metricsText(e)
	if !strings.Contains(m, `idp_cmi_embed_failures_total{kind="unavailable",role="query"} 2`) {
		t.Fatalf("query outage not counted:\n%s", m)
	}
	// the endpoint is back: the gauge moves
	docs.setErr(nil)
	if n, err := e.store.BackfillEmbeddings(ctx, 100); err != nil || n < 1 {
		t.Fatalf("%d %v", n, err)
	}
	if strings.Contains(metricsText(e), `idp_cmi_embedding_last_success_timestamp_seconds{role="documents"} 0`) {
		t.Fatal("the last success of the documents role did not move")
	}
}
