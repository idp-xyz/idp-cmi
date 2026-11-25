package integration

import (
	"fmt"
	"strings"
	"testing"
)

func TestKnowledgeBasesAreScoped(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	e.must(200, "PUT", "/api/v1/units", "alice", map[string]any{"kind": "team", "id": "lane", "displayName": "Lane", "members": []string{"carol"}, "roles": []string{}})

	e.must(422, "POST", "/api/v1/knowledge/bases", "alice", kbBase("bob-notes", "subject", "bob", "handbook", nil, 0, 0))
	e.must(201, "POST", "/api/v1/knowledge/bases", "alice", kbBase("bob-notes", "subject", "bob", "member-notes", nil, 0, 0))
	e.must(409, "POST", "/api/v1/knowledge/bases", "alice", kbBase("bob-notes-2", "subject", "bob", "member-notes", nil, 0, 0)) // one personal base per member
	e.must(201, "POST", "/api/v1/knowledge/bases", "alice", kbBase("lane-docs", "team", "lane", "handbook", nil, 0, 0))
	e.must(201, "POST", "/api/v1/knowledge/bases", "alice", kbBase("tiny", "tenant", "", "handbook", nil, 1, 0))
	// the name prefix is the service's
	e.must(422, "POST", "/api/v1/knowledge/bases", "alice", kbBase("personal-carol", "tenant", "", "handbook", nil, 0, 0))

	got := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://enterprise", "", "enterprise gammaquartz policy"))
	if got["knowledgeBase"] != "handbook" {
		t.Fatalf("default base: %v", got["knowledgeBase"])
	}
	e.must(201, "POST", "/v1/knowledge/documents", "bob", kbDoc("member-notes", "kb://bob-notes/notebook", "bob-notes", "bobprivate alphaquartz notebook"))
	e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "kb://lane-docs/schedule", "lane-docs", "laneteam betaquartz schedule"))
	e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "kb://tiny/first", "tiny", "tiny first document"))
	st, out := e.call("POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "kb://tiny/second", "tiny", "tiny second document"))
	mustFail(t, st, out, 429, "QUOTA_EXCEEDED")

	if !kbSees(e, "carol", "gammaquartz") || !kbSees(e, "carol", "betaquartz") || kbSees(e, "carol", "alphaquartz") {
		t.Fatal("carol's view of the three bases is wrong")
	}
	if !kbSees(e, "bob", "gammaquartz") || !kbSees(e, "bob", "alphaquartz") || kbSees(e, "bob", "betaquartz") {
		t.Fatal("bob's view of the three bases is wrong")
	}
	e.must(404, "GET", "/api/v1/knowledge/bases/bob-notes", "carol", nil)
	e.must(200, "GET", "/api/v1/knowledge/bases/bob-notes", "bob", nil)
	e.must(200, "GET", "/api/v1/knowledge/bases/bob-notes", "alice", nil)
	if auditCount(e, "knowledge_base.read", "") != 1 {
		t.Fatal("administrator read of a personal base was not audited exactly once (not the owner's)")
	}

	e.must(200, "POST", "/api/v1/knowledge/bases/lane-docs/archive", "alice", nil)
	if kbSees(e, "carol", "betaquartz") {
		t.Fatal("archived base still retrieved")
	}
	for _, who := range []string{"carol", "bob"} {
		for _, b := range e.must(200, "GET", "/api/v1/knowledge/bases", who, nil)["knowledgeBases"].([]any) {
			name := b.(map[string]any)["name"]
			if name == "lane-docs" || (who == "carol" && name == "bob-notes") {
				t.Fatalf("%s listed %v", who, name)
			}
		}
	}
	// the audit of an archive names the base by its id, like the creation does
	var created, archived string
	for _, ev := range e.must(200, "GET", "/api/v1/audit?limit=1000", "alice", nil)["events"].([]any) {
		m := ev.(map[string]any)
		if m["detail"] != nil && m["detail"].(map[string]any)["name"] == "lane-docs" {
			switch m["action"] {
			case "knowledge_base.create":
				created, _ = m["resourceId"].(string)
			case "knowledge_base.archive":
				archived, _ = m["resourceId"].(string)
			}
		}
	}
	if created == "" || created != archived {
		t.Fatalf("audit ids: create %q, archive %q", created, archived)
	}
}

// a unit member who is not a manager cannot write the unit's base, and the sensitivity ceilings of the base and of the writer hold.
func TestKnowledgeBaseManagersAndCeilings(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	e.must(200, "PUT", "/api/v1/units", "alice", map[string]any{"kind": "team", "id": "lane", "displayName": "Lane", "members": []string{"carol", "bob"}, "roles": []string{}})
	lane := kbBase("lane-docs", "team", "lane", "handbook", map[string]any{"subjects": []string{"carol"}}, 0, 0)
	lane["sensitivityCeiling"] = "confidential"
	lane["defaultSensitivity"] = "confidential"
	e.must(201, "POST", "/api/v1/knowledge/bases", "alice", lane)
	e.must(201, "POST", "/api/v1/knowledge/bases", "alice", kbBase("ent", "tenant", "", "handbook", map[string]any{"roles": []string{"finance"}}, 0, 0))

	// bob is a member of the unit and reads its base but does not manage it: 403, not 404
	st, out := e.call("POST", "/v1/knowledge/documents", "bob", kbDoc("handbook", "kb://lane-docs/bob", "lane-docs", "bob is no manager"))
	mustFail(t, st, out, 403, "FORBIDDEN")
	// erin of another tenant, or dan outside the unit, does not even see it
	st, out = e.call("POST", "/v1/knowledge/documents", "dan", kbDoc("handbook", "kb://lane-docs/dan", "lane-docs", "dan is outside"))
	mustFail(t, st, out, 404, "NOT_FOUND")
	// a manager by subject writes
	e.must(201, "POST", "/v1/knowledge/documents", "carol", kbDoc("handbook", "kb://lane-docs/carol", "lane-docs", "carol manages"))
	// a manager by role writes (dan has the role finance), another reader does not
	e.must(201, "POST", "/v1/knowledge/documents", "dan", kbDoc("handbook", "kb://ent/dan", "ent", "dan writes by role"))
	st, out = e.call("POST", "/v1/knowledge/documents", "carol", kbDoc("handbook", "kb://ent/carol", "ent", "carol has no role"))
	mustFail(t, st, out, 403, "FORBIDDEN")

	// the base ceiling: confidential is the highest the lane base takes, even for an administrator
	over := kbDoc("handbook", "kb://lane-docs/secret", "lane-docs", "restricted material")
	over["sensitivity"] = "restricted"
	st, out = e.call("POST", "/api/v1/knowledge/documents", "alice", over)
	mustFail(t, st, out, 422, "INVALID")
	over["sensitivity"] = "confidential"
	e.must(201, "POST", "/api/v1/knowledge/documents", "alice", over)
	// the writer's ceiling: a reader's is internal, so a manager who is a reader cannot write confidential content
	st, out = e.call("POST", "/v1/knowledge/documents", "carol", over)
	mustFail(t, st, out, 403, "FORBIDDEN")
	// defaultSensitivity applies when the upload names none
	none := kbDoc("handbook", "kb://lane-docs/plain", "lane-docs", "no sensitivity named")
	delete(none, "sensitivity")
	doc := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", none)
	if doc["sensitivity"] != "confidential" {
		t.Fatalf("default sensitivity: %v", doc["sensitivity"])
	}
}

// personal bases: found by owner, named safely from any subject, reserved prefix, and an owner reopens an archived one.
func TestPersonalBaseNamingAndUnarchive(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	first := e.must(201, "POST", "/v1/knowledge/documents", "bob", kbDoc("member-notes", "kb://me/first", "", "bobfirst quartzone"))
	name := ownBase(e, "bob")
	if !strings.HasPrefix(name, "personal-bob-") || first["knowledgeBase"] != name || first["uri"] != "kb://"+name+"/first" {
		t.Fatalf("personal base %q, document %v", name, first)
	}
	// the same base, found by its owner, is used again; its uri may be spelled with the name too
	second := e.must(201, "POST", "/v1/knowledge/documents", "bob", kbDoc("member-notes", "kb://"+name+"/second", "", "bobsecond quartztwo"))
	if second["knowledgeBase"] != name {
		t.Fatalf("second upload went to %v", second["knowledgeBase"])
	}
	// any other spelling of the uri is refused with a clear 422
	for _, uri := range []string{"notes://bob/x", "handbook://x", "kb://other/x", "kb://me"} {
		st, out := e.call("POST", "/v1/knowledge/documents", "bob", kbDoc("member-notes", uri, "", "text"))
		if st != 422 || !strings.Contains(fmt.Sprint(out), "kb://") {
			t.Fatalf("%s: %d %v", uri, st, out)
		}
	}
	// archived by an administrator: the owner is locked out of uploads, can reopen it, and nobody else can
	e.must(200, "POST", "/api/v1/knowledge/bases/"+name+"/archive", "alice", nil)
	st, out := e.call("POST", "/v1/knowledge/documents", "bob", kbDoc("member-notes", "kb://me/third", "", "bobthird"))
	mustFail(t, st, out, 409, "CONFLICT")
	if kbSees(e, "bob", "quartzone") {
		t.Fatal("an archived personal base is retrieved")
	}
	e.must(404, "POST", "/api/v1/knowledge/bases/"+name+"/unarchive", "carol", nil)
	e.must(200, "POST", "/api/v1/knowledge/bases/"+name+"/unarchive", "bob", nil)
	e.must(201, "POST", "/v1/knowledge/documents", "bob", kbDoc("member-notes", "kb://me/third", "", "bobthird quartzthree"))
	if !kbSees(e, "bob", "quartzone") {
		t.Fatal("a reopened personal base is not retrieved")
	}
	e.must(409, "POST", "/api/v1/knowledge/bases/"+name+"/unarchive", "bob", nil) // it is active now

	// a subject with capitals and punctuation gets a base too, and two subjects that fold alike get two
	for _, who := range []string{"Zoe.O'Neil@Example.com|x", "zoe-o-neil-example-com-x"} {
		if err := e.addUser(who); err != nil {
			t.Fatal(err)
		}
		e.must(201, "POST", "/v1/knowledge/documents", who, kbDoc("member-notes", "kb://me/n", "", "note by "+who))
	}
	a, b := ownBase(e, "Zoe.O'Neil@Example.com|x"), ownBase(e, "zoe-o-neil-example-com-x")
	if a == b || !strings.HasPrefix(a, "personal-zoe-o-neil-example-com-x-") {
		t.Fatalf("names %q %q", a, b)
	}
}
