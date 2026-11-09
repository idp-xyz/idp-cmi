package integration

import (
	"fmt"
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
