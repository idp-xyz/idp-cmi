package integration

import (
	"strings"
	"testing"
)

// The words of a heading are indexed whatever the settings (the heading is not in the body, and a section must be found by its
// heading: HEAD found it, the first draft did not). Only the document title in front of them is the setting
// `knowledge.indexContext`, off by default (ADR-0001 D56). The excerpt is the body, under a line with the heading path, and the
// section is also in the item's locator.
func TestKnowledgeHeadingIndexSetting(t *testing.T) {
	for _, titleOn := range []bool{false, true} {
		e := newEnv(t) // the setting is what Open starts with when it is not set here: off
		if titleOn {
			e.store.Knowledge.IndexContext = true
		}
		kbSources(e)
		e.must(201, "POST", "/api/v1/knowledge/documents", "alice", map[string]any{
			"source": "handbook", "uri": "handbook://fuel", "title": "Pricebookzq", "sensitivity": "internal", "acl": map[string]any{},
			"text": "# QzxHeading\n\nThe QzxBody surcharge is waived on weekends.\n\n## Weekendzq deliveries\n\nSaturday is free.\n\nTotal amount due: 1,234 EUR\n---\n\nThe fee.\n",
		})
		for _, q := range []string{"QzxHeading", "weekendzq", "amount"} {
			if got := items(e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": q, "kinds": []string{"knowledge"}})); len(got) == 0 {
				t.Fatalf("title in the index %v: a section cannot be found by its heading word %q", titleOn, q)
			}
		}
		byTitle := items(e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": "Pricebookzq", "kinds": []string{"knowledge"}}))
		if (len(byTitle) > 0) != titleOn {
			t.Fatalf("title in the index %v: a title word returned %d items", titleOn, len(byTitle))
		}
		byBody := items(e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": "QzxBody", "kinds": []string{"knowledge"}}))
		if len(byBody) != 1 {
			t.Fatalf("body search returned %d items", len(byBody))
		}
		excerpt := byBody[0]["excerpt"].(string)
		if !strings.HasPrefix(excerpt, "QzxHeading\n") || !strings.Contains(excerpt, "QzxBody") || strings.Contains(excerpt, "Pricebookzq") {
			t.Fatalf("excerpt %q", excerpt)
		}
		loc, _ := byBody[0]["locator"].(map[string]any)
		if loc == nil || len(loc["headings"].([]any)) != 1 || loc["headings"].([]any)[0] != "QzxHeading" {
			t.Fatalf("locator %v", byBody[0]["locator"])
		}
	}
}
