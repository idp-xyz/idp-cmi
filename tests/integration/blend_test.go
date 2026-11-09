package integration

import (
	"context"
	"errors"
	"fmt"
	"idp-cmi/internal/rerank"
	"strings"
	"testing"
)

// Semantic-led relevance (ADR-0001 D37): with Blend a word match alone does not keep a candidate whose
// vector is far from the query, so a query with no answer returns nothing; meaning still finds a
// document that shares no word with the query.
func TestBlendedRelevance(t *testing.T) {
	e := newEnv(t)
	e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "assistant", "displayName": "Assistant", "kind": "agent", "trust": "informational"})
	for i, c := range []string{"Morning pickup slots run from 07:00 to 11:00 every weekday", "Customs duties are prepaid when the sender chooses DDP shipping"} {
		e.must(201, "POST", "/v1/memory/records", "bob", memory("assistant", fmt.Sprintf("r%d", i), "tenant", "", c, "internal", e.now))
	}
	if n, err := e.store.BackfillEmbeddings(t.Context(), 100); err != nil || n != 0 { // writes embed synchronously
		t.Fatalf("backfill %d %v", n, err)
	}
	ask := func(q string) string {
		out := e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": q, "purpose": "test"})
		var b strings.Builder
		for _, it := range items(out) {
			b.WriteString(fmt.Sprint(it["excerpt"]) + "|")
		}
		return b.String()
	}
	noise := "the weather forecast for duties next week" // shares only the word "duties" with the customs record
	if got := ask(noise); !strings.Contains(got, "Customs duties") {
		t.Fatalf("legacy relevance keeps a word match: %q", got)
	}
	e.host.Resolver.Blend = 0.1
	defer func() { e.host.Resolver.Blend = 0 }()
	if got := ask(noise); got != "" {
		t.Fatalf("a word match with a distant vector must be dropped: %q", got)
	}
	if got := ask("collection slots"); !strings.Contains(got, "pickup slots") { // "collection" means "pickup" to the fake embedder
		t.Fatalf("meaning finds the record without a shared word: %q", got)
	}
	e.host.Resolver.SemanticFloor = 0.99
	if got := ask("pickup slots weekday morning"); got != "" {
		t.Fatalf("a floor above every cosine returns nothing: %q", got)
	}
	e.host.Resolver.SemanticFloor = 0
}

type failingReranker struct{}

func (failingReranker) Model() string { return "down" }
func (failingReranker) Rerank(context.Context, string, []string) ([]float64, error) {
	return nil, errors.New("reranker unreachable")
}

// A cross-encoder (ADR-0001 D39) decides "nothing here answers it" with one threshold, and its failure
// does not take retrieval down.
func TestRerankStage(t *testing.T) {
	e := newEnv(t)
	e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "assistant", "displayName": "Assistant", "kind": "agent", "trust": "informational"})
	for i, c := range []string{"Morning pickup slots run from 07:00 to 11:00 every weekday", "Customs duties are prepaid when the sender chooses DDP shipping", "Pickup outside the booked slot costs a 20 EUR surcharge"} {
		e.must(201, "POST", "/v1/memory/records", "bob", memory("assistant", fmt.Sprintf("r%d", i), "tenant", "", c, "internal", e.now))
	}
	ask := func(q string) []string {
		var out []string
		for _, it := range items(e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": q, "purpose": "test"})) {
			out = append(out, fmt.Sprint(it["excerpt"]))
		}
		return out
	}
	e.host.Resolver.Reranker, e.host.Resolver.RerankFloor = rerank.Fake{}, 0.5
	defer func() { e.host.Resolver.Reranker, e.host.Resolver.RerankFloor = nil, 0 }()
	got := ask("pickup slots weekday")
	if len(got) != 1 || !strings.Contains(got[0], "Morning pickup slots") {
		t.Fatalf("the reranker keeps what answers the query: %v", got)
	}
	if got := ask("the weather forecast for duties next week"); len(got) != 0 {
		t.Fatalf("under the floor nothing is returned: %v", got)
	}
	e.host.Resolver.RerankFloor = 0
	if got := ask("pickup slots"); len(got) < 2 || !strings.Contains(got[0], "pickup") {
		t.Fatalf("without a floor the reranker only orders: %v", got)
	}
	e.host.Resolver.Reranker = failingReranker{}
	if got := ask("pickup slots weekday"); len(got) == 0 {
		t.Fatal("a failing reranker must not take retrieval down")
	}
}
