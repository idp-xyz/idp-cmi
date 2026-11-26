package integration

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"idp-cmi/adapters/blob"
	"idp-cmi/internal/pdp"
)

// D53 (S1): the default scope is the enterprise bases, the caller's units and the caller's own personal base - an
// administrator's too. Another member's personal base is read by naming it, and every such read is audited as
// knowledge.read_other: the chunks of a resolve, the document, the list, the original.
func TestAdministratorReadsAPersonalBaseOnlyByNamingIt(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	mem := &blob.Mem{}
	e.store.Originals = mem
	e.must(200, "PUT", "/api/v1/units", "alice", map[string]any{"kind": "team", "id": "lane", "displayName": "Lane", "members": []string{"carol"}, "roles": []string{}})
	e.must(201, "POST", "/api/v1/knowledge/bases", "alice", kbBase("lane-docs", "team", "lane", "handbook", nil, 0, 0))
	e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "kb://lane-docs/plan", "lane-docs", "laneonly sigmaquartz"))
	e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://open", "", "enterprise sigmaopen"))
	bob := e.must(201, "POST", "/v1/knowledge/documents", "bob", kbDoc("member-notes", "kb://me/diary", "", "bobdiary sigmapersonal"))
	id, base := bob["documentId"].(string), bob["knowledgeBase"].(string)
	if st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "application/pdf", []byte("%PDF diary")); st != 200 {
		t.Fatal(st)
	}
	named := map[string]any{"kind": "subject", "id": "bob"}

	// resolve: not in the default scope of the administrator - or of anyone else
	if kbSees(e, "alice", "sigmapersonal") || kbSees(e, "carol", "sigmapersonal") {
		t.Fatal("another member's personal base is in a default scope")
	}
	if !kbSees(e, "alice", "sigmaopen") || !kbSees(e, "bob", "sigmapersonal") {
		t.Fatal("the default scope lost the enterprise base or the owner's own base")
	}
	// the administrator is in no unit: the unit's base is not in the default scope either
	if kbSees(e, "alice", "laneonly") || !kbSees(e, "carol", "laneonly") {
		t.Fatal("unit base scope")
	}
	if auditCount(e, "knowledge.read_other", "") != 0 {
		t.Fatal("audited before any read")
	}
	// naming the member's subject scope: the administrator reads it, audited once per resolve with chunks of it
	if !kbSees(e, "alice", "sigmapersonal", named) {
		t.Fatal("the administrator naming the scope cannot read the base")
	}
	if n := auditCount(e, "knowledge.read_other", ""); n != 1 {
		t.Fatalf("audit events after a named resolve: %d", n)
	}
	// naming it but matching nothing of it reads nothing, and records nothing
	if !kbSees(e, "alice", "sigmaopen", named) || auditCount(e, "knowledge.read_other", "") != 1 {
		t.Fatal("a resolve that names the scope but returns none of its chunks was audited")
	}
	// a reader may not name another member's scope; the owner naming their own is not "other"
	if st, _ := e.call("POST", "/v1/context/resolve", "carol", map[string]any{"query": "x", "kinds": []string{"knowledge"}, "scopes": []any{named}}); st != 403 {
		t.Fatalf("reader naming bob: %d", st)
	}
	if !kbSees(e, "bob", "sigmapersonal", named) || auditCount(e, "knowledge.read_other", "") != 1 {
		t.Fatal("the owner naming their own scope")
	}

	// documents by id: 404 for a member, a named read for the administrator, audited
	e.must(404, "GET", "/v1/knowledge/documents/"+id, "carol", nil)
	e.must(200, "GET", "/v1/knowledge/documents/"+id, "bob", nil)
	before := auditCount(e, "knowledge.read_other", "")
	// by its id alone the administrator gets the 404 of a document that is not there; naming the member serves it (D78)
	e.must(404, "GET", "/v1/knowledge/documents/"+id, "alice", nil)
	e.must(404, "GET", "/api/v1/knowledge/documents/"+id, "alice", nil)
	e.must(200, "GET", "/v1/knowledge/documents/"+id+"?subject=bob", "alice", nil)
	e.must(200, "GET", "/api/v1/knowledge/documents/"+id+"?subject=bob", "alice", nil)
	if auditCount(e, "knowledge.read_other", "") != before+2 {
		t.Fatalf("by-id reads were audited %d times, want 2", auditCount(e, "knowledge.read_other", "")-before)
	}
	// the administrator's list leaves the personal bases out unless one is named
	list := func(path string) string {
		return fmt.Sprint(e.must(200, "GET", path, "alice", nil)["documents"])
	}
	if strings.Contains(list("/api/v1/knowledge/documents"), "diary") || strings.Contains(list("/v1/knowledge/documents"), "diary") {
		t.Fatal("the administrator's list shows another member's documents")
	}
	before = auditCount(e, "knowledge.read_other", "")
	if !strings.Contains(list("/api/v1/knowledge/documents?knowledgeBase="+base), "diary") || auditCount(e, "knowledge.read_other", "") != before+1 {
		t.Fatal("naming the base in the list")
	}
	// the original
	if st, _, _ := e.callBytes(http.MethodGet, "/v1/knowledge/documents/"+id+"/original", "carol", "", nil); st != 404 {
		t.Fatalf("carol's download: %d", st)
	}
	before = auditCount(e, "knowledge.read_other", "")
	if st, _, _ := e.callBytes(http.MethodGet, "/v1/knowledge/documents/"+id+"/original", "alice", "", nil); st != 404 {
		t.Fatalf("administrator's download without naming the member: %d", st)
	}
	if st, _, body := e.callBytes(http.MethodGet, "/v1/knowledge/documents/"+id+"/original?subject=bob", "alice", "", nil); st != 200 || string(body) != "%PDF diary" {
		t.Fatalf("administrator's download: %d", st)
	}
	if auditCount(e, "knowledge.read_other", "") != before+1 {
		t.Fatal("the administrator's download was not audited")
	}
	// the audit entry says who read whose
	for _, ev := range e.must(200, "GET", "/api/v1/audit?limit=1000", "alice", nil)["events"].([]any) {
		m := ev.(map[string]any)
		if m["action"] == "knowledge.read_other" {
			d := m["detail"].(map[string]any)
			if m["subject"] != "alice" || d["subject"] != "bob" || d["via"] == nil {
				t.Fatalf("audit entry %v", m)
			}
		}
	}
	// an administrator does not write into a member's personal base, but may withdraw a document of it
	st, out := e.call("POST", "/api/v1/knowledge/documents", "alice", kbDoc("member-notes", "kb://"+base+"/injected", base, "no"))
	mustFail(t, st, out, 404, "NOT_FOUND")
	e.must(200, "DELETE", "/api/v1/knowledge/documents/"+id+"?reason=policy", "alice", nil)
}

// D23 (S2): every member route asks the PDP, and its obligations hold for lists, reads and downloads.
func TestMemberKnowledgeRoutesAskThePDP(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	e.store.Originals = &blob.Mem{}
	e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://pdp-ent", "", "pdpentquartz"))
	doc := e.must(201, "POST", "/v1/knowledge/documents", "bob", kbDoc("member-notes", "kb://me/a", "", "pdpquartz"))
	id := doc["documentId"].(string)
	if st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "application/pdf", []byte("%PDF pdp")); st != 200 {
		t.Fatal(st)
	}
	answer := `{"result": {"allow": false, "reason": "probe"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(answer)) }))
	defer srv.Close()
	e.host.PDP = &pdp.Client{URL: srv.URL, HTTP: srv.Client()}

	routes := []struct {
		method, path, media string
		body                []byte
	}{
		{"GET", "/v1/knowledge/documents", "", nil},
		{"GET", "/v1/knowledge/documents/" + id, "", nil},
		{"DELETE", "/v1/knowledge/documents/" + id, "", nil},
		{"PUT", "/v1/knowledge/documents/" + id + "/original", "application/pdf", []byte("%PDF again")},
		{"GET", "/v1/knowledge/documents/" + id + "/original", "", nil},
	}
	for _, r := range routes {
		st, _, body := e.callBytes(r.method, r.path, "bob", r.media, r.body)
		if st != 403 || !strings.Contains(string(body), "PDP_DENIED") {
			t.Errorf("%s %s under a deny-all PDP: %d %s", r.method, r.path, st, body)
		}
	}
	// obligations: excluded sources and a lower sensitivity ceiling hide the document from every member route
	for name, obligations := range map[string]string{
		"excludeSources": `"excludeSources": ["member-notes"]`,
		"maxSensitivity": `"maxSensitivity": "public"`,
	} {
		answer = `{"result": {"allow": true, "obligations": {` + obligations + `}}}`
		if st, _, _ := e.callBytes("GET", "/v1/knowledge/documents/"+id, "bob", "", nil); st != 404 {
			t.Errorf("%s: GET document %d", name, st)
		}
		if st, _, _ := e.callBytes("GET", "/v1/knowledge/documents/"+id+"/original", "bob", "", nil); st != 404 {
			t.Errorf("%s: GET original %d", name, st)
		}
		if st, _, _ := e.callBytes("PUT", "/v1/knowledge/documents/"+id+"/original", "bob", "application/pdf", []byte("%PDF again")); st != 404 {
			t.Errorf("%s: PUT original %d", name, st)
		}
		if st, _, _ := e.callBytes("DELETE", "/v1/knowledge/documents/"+id, "bob", "", nil); st != 404 {
			t.Errorf("%s: DELETE %d", name, st)
		}
		_, _, body := e.callBytes("GET", "/v1/knowledge/documents", "bob", "", nil)
		if strings.Contains(string(body), id) {
			t.Errorf("%s: the list shows the document: %s", name, body)
		}
	}
	// and with no obligations it works
	answer = `{"result": {"allow": true}}`
	if st, _, _ := e.callBytes("GET", "/v1/knowledge/documents/"+id+"/original", "bob", "", nil); st != 200 {
		t.Fatalf("allowed: %d", st)
	}
}

// S10: list and revoke apply the visibility of reads - ACL, sensitivity, erasure - and a stale If-Match is a 409 on both paths.
func TestMemberListAndRevokeFollowReadVisibility(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	e.must(200, "PUT", "/api/v1/units", "alice", map[string]any{"kind": "team", "id": "lane", "displayName": "Lane", "members": []string{"carol", "dan"}, "roles": []string{}})
	e.must(201, "POST", "/api/v1/knowledge/bases", "alice", kbBase("lane-docs", "team", "lane", "handbook", map[string]any{"subjects": []string{"carol"}}, 0, 0))
	post := func(uri, text, sens string, acl map[string]any) string {
		d := kbDoc("handbook", "kb://lane-docs/"+uri, "lane-docs", text)
		d["sensitivity"] = sens
		d["acl"] = acl
		return e.must(201, "POST", "/api/v1/knowledge/documents", "alice", d)["documentId"].(string)
	}
	open := post("open", "openlane", "internal", map[string]any{})
	finance := post("finance", "financelane", "internal", map[string]any{"roles": []string{"finance"}})
	secret := post("secret", "secretlane", "restricted", map[string]any{})
	list := func(who string) string {
		return fmt.Sprint(e.must(200, "GET", "/v1/knowledge/documents", who, nil)["documents"])
	}
	got := list("carol") // a manager of the base, a reader of internal content, without the finance role
	if !strings.Contains(got, open) || strings.Contains(got, finance) || strings.Contains(got, secret) {
		t.Fatalf("carol's list shows what she may not read: %s", got)
	}
	// the manager cannot withdraw what she cannot read
	e.must(404, "DELETE", "/v1/knowledge/documents/"+finance, "carol", nil)
	e.must(404, "DELETE", "/v1/knowledge/documents/"+secret, "carol", nil)
	// the administrator can (the administrator's route), and the document is withdrawn
	e.must(200, "DELETE", "/api/v1/knowledge/documents/"+finance, "alice", nil)

	// If-Match: 409 on both routes when it does not match, and the document is not revoked
	rev := int(e.must(200, "GET", "/v1/knowledge/documents/"+open, "carol", nil)["revision"].(float64))
	e.must(409, "DELETE", "/v1/knowledge/documents/"+open, "carol", nil, "If-Match", fmt.Sprint(rev+5))
	e.must(409, "DELETE", "/api/v1/knowledge/documents/"+open, "alice", nil, "If-Match", fmt.Sprint(rev+5))
	if e.must(200, "GET", "/v1/knowledge/documents/"+open, "carol", nil)["status"] != "active" {
		t.Fatal("a conflicting revoke changed the document")
	}
	e.must(200, "DELETE", "/v1/knowledge/documents/"+open, "carol", nil, "If-Match", fmt.Sprint(rev))
	// a document that is already withdrawn is not found, with or without If-Match
	e.must(404, "DELETE", "/v1/knowledge/documents/"+open, "carol", nil, "If-Match", fmt.Sprint(rev))
	// erased documents are in nobody's list
	gone := post("gone", "erasedlane", "internal", map[string]any{})
	e.must(201, "POST", "/api/v1/erasures", "alice", map[string]any{"documentIds": []string{gone}, "reason": "x"})
	if strings.Contains(list("carol"), gone) {
		t.Fatal("an erased document is listed")
	}
	e.must(404, "GET", "/v1/knowledge/documents/"+gone, "carol", nil)
}
