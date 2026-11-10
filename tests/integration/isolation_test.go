package integration

import (
	"fmt"
	"strings"
	"testing"
)

// Subject isolation (ADR-0001 D42): another member's personal memory is not readable by a reader, however it is asked for.
func TestSubjectMemoryIsPrivateToItsSubject(t *testing.T) {
	e := newEnv(t)
	e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "assistant", "displayName": "A", "kind": "agent", "trust": "informational"})
	rec := e.must(201, "POST", "/v1/memory/records", "bob", memory("assistant", "r1", "subject", "carol", "Carol's favourite colour is teal and she lives in Chengdu", "internal", e.now))
	id := rec["recordId"].(string)
	problem := func(out map[string]any) string {
		m, _ := out["error"].(map[string]any)
		s, _ := m["message"].(string)
		return s
	}

	t.Run("a-reader-cannot-list-another-members-scope", func(t *testing.T) {
		st, out := e.call("GET", "/v1/memory/records?scopeKind=subject&scopeId=carol", "dan", nil)
		if st != 403 || !strings.Contains(problem(out), "another member") {
			t.Fatalf("%d %v", st, out)
		}
		if n := len(e.must(200, "GET", "/v1/memory/records?scopeKind=subject&scopeId=dan", "dan", nil)["records"].([]any)); n != 0 {
			t.Fatalf("dan's own scope is his own: %d", n)
		}
		if n := len(e.must(200, "GET", "/v1/memory/records", "carol", nil)["records"].([]any)); n != 1 {
			t.Fatalf("carol sees hers: %d", n)
		}
	})

	t.Run("a-reader-cannot-resolve-another-members-scope", func(t *testing.T) {
		for name, body := range map[string]map[string]any{
			"scope":  {"query": "Carol favourite colour Chengdu", "purpose": "t", "scope": map[string]any{"kind": "subject", "id": "carol"}},
			"scopes": {"query": "Carol favourite colour Chengdu", "purpose": "t", "scopes": []map[string]any{{"kind": "subject", "id": "carol"}}},
		} {
			if st, out := e.call("POST", "/v1/context/resolve", "dan", body); st != 403 {
				t.Fatalf("%s: %d %v", name, st, out)
			}
		}
		// his own subject scope and the default resolve are fine and do not carry carol's memory
		own := e.must(200, "POST", "/v1/context/resolve", "dan", map[string]any{"query": "Carol favourite colour Chengdu", "purpose": "t", "scope": map[string]any{"kind": "subject", "id": "dan"}})
		for _, it := range items(own) {
			if strings.Contains(it["excerpt"].(string), "Carol") {
				t.Fatalf("carol's memory in dan's package: %v", it)
			}
		}
	})

	t.Run("a-reader-cannot-read-the-record-by-id", func(t *testing.T) {
		if st, _ := e.call("GET", "/v1/memory/records/"+id, "dan", nil); st != 404 {
			t.Fatalf("by id as dan: %d", st)
		}
		if st, _ := e.call("GET", "/v1/memory/records/"+id, "carol", nil); st != 200 {
			t.Fatalf("by id as carol: %d", st)
		}
	})

	t.Run("reads-of-another-members-memory-are-audited", func(t *testing.T) {
		reads := func() []map[string]any {
			var out []map[string]any
			for _, ev := range e.must(200, "GET", "/api/v1/audit?limit=500", "alice", nil)["events"].([]any) {
				if m := ev.(map[string]any); m["action"] == "memory.read_other" {
					out = append(out, m)
				}
			}
			return out
		}
		before := len(reads())
		e.must(200, "GET", "/v1/memory/records?scopeKind=subject&scopeId=carol", "bob", nil)
		e.must(200, "GET", "/v1/memory/records?scopeKind=subject&scopeId=bob", "bob", nil) // his own: nothing to record
		after := reads()
		if len(after) != before+1 {
			t.Fatalf("one audit event for reading carol's scope, none for his own: %d -> %d", before, len(after))
		}
		if ev := after[0]; ev["subject"] != "bob" || ev["resourceId"] != "carol" || !strings.Contains(fmt.Sprint(ev["detail"]), "list") {
			t.Fatalf("%v", ev)
		}
	})

	t.Run("the-writing-service-and-an-administrator-can", func(t *testing.T) {
		for _, who := range []string{"bob", "alice"} {
			if n := len(e.must(200, "GET", "/v1/memory/records?scopeKind=subject&scopeId=carol", who, nil)["records"].([]any)); n != 1 {
				t.Fatalf("%s: %d", who, n)
			}
			if st, _ := e.call("GET", "/v1/memory/records/"+id, who, nil); st != 200 {
				t.Fatalf("%s by id: %d", who, st)
			}
		}
	})
}
