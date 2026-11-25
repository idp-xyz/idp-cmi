package integration

import (
	"context"
	"strings"
	"testing"

	"idp-cmi/internal/cmi"
)

// B1: a version's identity is (tenant, knowledge base, uri). No upload into one base can retire, renumber or hide a
// document of another, in any direction.
func TestVersionIdentityIsPerBase(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	e.must(200, "PUT", "/api/v1/units", "alice", map[string]any{"kind": "team", "id": "lane", "displayName": "Lane", "members": []string{"carol", "dan"}, "roles": []string{}})
	e.must(201, "POST", "/api/v1/knowledge/bases", "alice", kbBase("lane-docs", "team", "lane", "handbook", map[string]any{"subjects": []string{"carol"}}, 0, 0))

	ent := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://travel-policy", "", "enterprise zorbtravel policy"))
	if !kbSees(e, "dan", "zorbtravel") {
		t.Fatal("setup: the enterprise document is not seen")
	}
	stillThere := func(when string) {
		t.Helper()
		got := e.must(200, "GET", "/api/v1/knowledge/documents/"+ent["documentId"].(string), "alice", nil)
		if got["status"] != "active" || !kbSees(e, "dan", "zorbtravel") {
			t.Fatalf("%s: the enterprise document was retired or hidden (status %v)", when, got["status"])
		}
	}

	// personal -> enterprise: the uri of an enterprise document, in a member's upload, is refused with a clear 422
	for _, uri := range []string{"handbook://travel-policy", "notes://bob/travel-policy", "kb://handbook/travel-policy"} {
		st, out := e.call("POST", "/v1/knowledge/documents", "bob", kbDoc("member-notes", uri, "", "bob scribble"))
		if st != 422 || !strings.Contains(out["error"].(map[string]any)["message"].(string), "kb://") {
			t.Fatalf("%s: %d %v", uri, st, out)
		}
	}
	bobDoc := e.must(201, "POST", "/v1/knowledge/documents", "bob", kbDoc("member-notes", "kb://me/travel-policy", "", "bob scribble zorbpersonal"))
	if bobDoc["version"] != float64(1) {
		t.Fatalf("version in the personal base: %v", bobDoc["version"])
	}
	stillThere("after bob's personal upload")

	// unit -> enterprise: a unit manager cannot take over an enterprise uri
	st, out := e.call("POST", "/v1/knowledge/documents", "carol", kbDoc("handbook", "handbook://travel-policy", "lane-docs", "lane copy"))
	mustFail(t, st, out, 422, "INVALID")
	lane := e.must(201, "POST", "/v1/knowledge/documents", "carol", kbDoc("handbook", "kb://lane-docs/travel-policy", "lane-docs", "lane zorblane copy"))
	if lane["version"] != float64(1) {
		t.Fatalf("version in the lane base: %v", lane["version"])
	}
	stillThere("after the lane upload")

	// enterprise -> personal and enterprise -> unit: an administrator's upload that names another base's uri is refused, and
	// naming the enterprise base with its own spelling is another document than the lane's and the member's
	st, out = e.call("POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", bobDoc["uri"].(string), "", "enterprise copy of bob's uri"))
	mustFail(t, st, out, 422, "INVALID")
	st, out = e.call("POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "kb://lane-docs/travel-policy", "handbook", "enterprise copy of the lane's uri"))
	mustFail(t, st, out, 422, "INVALID")
	st, out = e.call("POST", "/api/v1/knowledge/documents", "alice", kbDoc("member-notes", "handbook://x", bobDoc["knowledgeBase"].(string), "an administrator writes bob's base"))
	mustFail(t, st, out, 404, "NOT_FOUND")
	named := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "kb://handbook/travel-policy", "handbook", "enterprise zorbnamed v2"))
	if named["version"] != float64(1) || named["knowledgeBase"] != "handbook" {
		t.Fatalf("a document named by its base is its own: %v", named)
	}
	// the three documents now coexist: the personal and unit ones did not renumber or retire each other
	if !kbSees(e, "dan", "zorblane") || !kbSees(e, "dan", "zorbnamed") || !kbSees(e, "dan", "zorbtravel") || !kbSees(e, "bob", "zorbpersonal") {
		t.Fatal("a document of one base hides another's")
	}
	// a second version of one of them retires only that one
	v2 := e.must(201, "POST", "/v1/knowledge/documents", "carol", kbDoc("handbook", "kb://lane-docs/travel-policy", "lane-docs", "lane zorblane2 copy"))
	if v2["version"] != float64(2) {
		t.Fatalf("lane version: %v", v2["version"])
	}
	stillThere("after the lane's second version")
	if !kbSees(e, "dan", "zorbnamed") || !kbSees(e, "dan", "zorblane2") {
		t.Fatal("the lane's new version, or the enterprise document of the same path, is missing")
	}
	old := e.must(200, "GET", "/api/v1/knowledge/documents/"+lane["documentId"].(string), "alice", nil)
	if old["status"] != "superseded" {
		t.Fatalf("the replaced lane version is %v", old["status"])
	}
}

// Documents that come in by connectors keep their uris and live in their source's base: the same uri in two
// sources' bases is two documents, and neither retires the other (the store path of a connector).
func TestConnectorURIsNeverCrossBases(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, name := range []string{"wiki", "drive"} {
		e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": name, "displayName": name, "kind": "document-store", "trust": "authoritative"})
	}
	mk := func(source, text string) *cmi.Document {
		d, err := e.store.CreateDocument(ctx, e.tenantA, cmi.Document{Source: source, URI: "shared://policy", Title: "Policy", Sensitivity: cmi.SensitivityInternal}, text, "connector:"+source)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	w1, d1 := mk("wiki", "wikipolicy first"), mk("drive", "drivepolicy first")
	if w1.Version != 1 || d1.Version != 1 || w1.KnowledgeBase != "wiki" || d1.KnowledgeBase != "drive" {
		t.Fatalf("versions %d %d bases %s %s", w1.Version, d1.Version, w1.KnowledgeBase, d1.KnowledgeBase)
	}
	w2 := mk("wiki", "wikipolicy second")
	if w2.Version != 2 {
		t.Fatalf("wiki version %d", w2.Version)
	}
	if !kbSees(e, "carol", "drivepolicy") || !kbSees(e, "carol", "wikipolicy") {
		t.Fatal("a version in one source's base hides a document of another's")
	}
	if got, err := e.store.ActiveDocumentDigest(ctx, e.tenantA, "drive", "shared://policy"); err != nil || got != d1.ContentSHA256 {
		t.Fatalf("drive digest %q %v", got, err)
	}
	if got, err := e.store.ActiveDocumentDigest(ctx, e.tenantA, "wiki", "shared://policy"); err != nil || got != w2.ContentSHA256 {
		t.Fatalf("wiki digest %q %v", got, err)
	}
	// the bitemporal view is per base too
	asOf := map[string]any{"valid": e.now.UTC().Format("2006-01-02T15:04:05Z")}
	got := items(e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": "wikipolicy drivepolicy", "kinds": []string{"knowledge"}, "asOf": asOf}))
	text := ""
	for _, it := range got {
		text += it["excerpt"].(string) + "|"
	}
	if !strings.Contains(text, "drivepolicy first") || !strings.Contains(text, "wikipolicy second") || strings.Contains(text, "wikipolicy first") {
		t.Fatalf("as-of view: %s", text)
	}
}
