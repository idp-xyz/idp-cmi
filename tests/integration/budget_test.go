package integration

import (
	"testing"
)

// A resolve that names no budget gets the default (ADR-0001 D5, D46): 3000 tokens, 20 items; a named budget is kept.
func TestDefaultBudget(t *testing.T) {
	e := newEnv(t)
	budget := func(body map[string]any) map[string]any {
		return e.must(200, "POST", "/v1/context/resolve", "carol", body)["package"].(map[string]any)["budget"].(map[string]any)
	}
	if b := budget(map[string]any{"query": "anything", "purpose": "t"}); b["maxTokens"] != 3000.0 || b["maxItems"] != 20.0 {
		t.Fatalf("default budget: %v", b)
	}
	if b := budget(map[string]any{"query": "anything", "purpose": "t", "budget": map[string]any{"maxTokens": 2000, "maxItems": 5}}); b["maxTokens"] != 2000.0 || b["maxItems"] != 5.0 {
		t.Fatalf("named budget: %v", b)
	}
}
