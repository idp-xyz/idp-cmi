package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMemberWriteAndValidityWindow(t *testing.T) {
	e := newEnv(t)
	e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "handbook", "displayName": "Handbook", "kind": "document-store", "trust": "authoritative"})
	e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "member-notes", "displayName": "Member notes", "kind": "user", "trust": "informational"})
	e.must(200, "PUT", "/api/v1/units", "alice", map[string]any{"kind": "team", "id": "lane", "displayName": "Lane", "members": []string{"carol"}, "roles": []string{}})
	e.must(201, "POST", "/api/v1/knowledge/bases", "alice", map[string]any{
		"name": "lane-docs", "displayName": "Lane", "scope": map[string]any{"kind": "team", "id": "lane"},
		"managers": map[string]any{"subjects": []string{"carol"}}, "defaultSensitivity": "internal", "sensitivityCeiling": "restricted",
		"source": "handbook", "quotaDocuments": 0, "quotaBytes": 0, "ocr": false,
	})

	bobDoc := e.must(201, "POST", "/v1/knowledge/documents", "bob", map[string]any{
		"source": "member-notes", "uri": "kb://me/sketch", "title": "Sketch", "sensitivity": "internal", "acl": map[string]any{},
		"docType": "note", "owner": "bob", "text": "bobsketch privatequartz",
	})
	if !strings.HasPrefix(fmt.Sprint(bobDoc["knowledgeBase"]), "personal-bob-") || bobDoc["docType"] != "note" || bobDoc["owner"] != "bob" {
		t.Fatalf("personal document: %v", bobDoc)
	}
	e.must(404, "GET", "/v1/knowledge/documents/"+bobDoc["documentId"].(string), "carol", nil)
	e.must(404, "POST", "/v1/knowledge/documents", "carol", map[string]any{
		"source": "member-notes", "uri": "kb://" + fmt.Sprint(bobDoc["knowledgeBase"]) + "/nope", "title": "Nope", "sensitivity": "internal", "acl": map[string]any{},
		"knowledgeBase": bobDoc["knowledgeBase"], "text": "should not land",
	})
	e.must(201, "POST", "/v1/knowledge/documents", "carol", map[string]any{
		"source": "handbook", "uri": "kb://lane-docs/lane-note", "title": "Lane note", "sensitivity": "internal", "acl": map[string]any{},
		"knowledgeBase": "lane-docs", "text": "lane manager note",
	})
	e.must(404, "POST", "/v1/knowledge/documents", "bob", map[string]any{
		"source": "handbook", "uri": "kb://lane-docs/bob-lane", "title": "No", "sensitivity": "internal", "acl": map[string]any{},
		"knowledgeBase": "lane-docs", "text": "bob cannot write the lane base",
	})
	listed := e.must(200, "GET", "/v1/knowledge/documents", "bob", nil)["documents"].([]any)
	if len(listed) != 1 || listed[0].(map[string]any)["knowledgeBase"] != bobDoc["knowledgeBase"] {
		t.Fatalf("bob's documents: %v", listed)
	}
	e.must(200, "DELETE", "/v1/knowledge/documents/"+bobDoc["documentId"].(string), "bob", nil)

	now := time.Now().UTC()
	uri := "handbook://rate-window"
	version := func(from, until time.Time, text string) {
		body := map[string]any{
			"source": "handbook", "uri": uri, "title": "Rate", "sensitivity": "internal", "acl": map[string]any{},
			"validFrom": from.Format(time.RFC3339), "text": text,
		}
		if !until.IsZero() {
			body["validUntil"] = until.Format(time.RFC3339)
		}
		e.must(201, "POST", "/api/v1/knowledge/documents", "alice", body)
	}
	version(now.Add(-30*24*time.Hour), now.Add(-20*24*time.Hour), "ratecard oldrate alpha")
	version(now.Add(-20*24*time.Hour), now.Add(-10*24*time.Hour), "ratecard midrate beta")
	version(now.Add(-10*24*time.Hour), now.Add(10*24*time.Hour), "ratecard nowrate gamma")
	version(now.Add(10*24*time.Hour), time.Time{}, "ratecard futurerate delta")

	see := func(asOf time.Time, want string, absent ...string) {
		t.Helper()
		body := map[string]any{"query": "ratecard", "kinds": []string{"knowledge"}, "sources": []string{"handbook"}}
		if !asOf.IsZero() {
			body["asOf"] = map[string]any{"valid": asOf.Format(time.RFC3339)}
		}
		got := fmt.Sprint(items(e.must(200, "POST", "/v1/context/resolve", "carol", body)))
		if !strings.Contains(got, want) {
			t.Fatalf("at %s want %s, got %s", asOf, want, got)
		}
		for _, word := range absent {
			if strings.Contains(got, word) {
				t.Fatalf("at %s did not want %s, got %s", asOf, word, got)
			}
		}
	}
	see(now.Add(-25*24*time.Hour), "oldrate", "midrate", "nowrate", "futurerate")
	see(now.Add(-15*24*time.Hour), "midrate", "oldrate", "nowrate", "futurerate")
	see(time.Time{}, "nowrate", "oldrate", "midrate", "futurerate")
}
