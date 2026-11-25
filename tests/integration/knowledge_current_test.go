package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// sqlAsOwner runs a statement as the schema owner: the tests move a version's window instead of sleeping until it opens.
func sqlAsOwner(t *testing.T, e *env, q string, args ...any) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), e.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

func resolveKnowledge(e *env, who, query string, extra map[string]any) map[string]any {
	e.t.Helper()
	body := map[string]any{"query": query, "kinds": []string{"knowledge"}}
	for k, v := range extra {
		body[k] = v
	}
	return e.must(200, "POST", "/v1/context/resolve", who, body)
}

func has(pkg map[string]any, word string) bool { return strings.Contains(fmt.Sprint(items(pkg)), word) }

func verify(e *env, who string, pkg map[string]any) map[string]any {
	e.t.Helper()
	return e.must(200, "POST", "/v1/context/packages/"+pkg["packageId"].(string)+"/verify", who, nil)
}

// D52: the in-force condition. A version whose validFrom has not come does not hide the one in force; when it comes,
// the old one is replaced - for retrieval, the admin list, quotas and package verification alike.
func TestNewVersionInForceCondition(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	doc := func(text string, from time.Time) map[string]any {
		b := kbDoc("handbook", "handbook://tariff", "", text)
		if !from.IsZero() {
			b["validFrom"] = from.UTC().Format(time.RFC3339)
		}
		return b
	}
	v1 := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", doc("tariffcard oldtariff", time.Time{}))
	if _, ok := v1["validFrom"]; ok {
		t.Fatalf("a document written without validFrom shows one: %v", v1["validFrom"])
	}
	v2 := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", doc("tariffcard newtariff", time.Now().Add(time.Hour)))
	// v2 is future-dated: v1 is still the current version, and says so
	p1 := resolveKnowledge(e, "carol", "tariffcard", nil)
	if !has(p1, "oldtariff") || has(p1, "newtariff") {
		t.Fatalf("before v2 is in force: %v", items(p1))
	}
	if got := e.must(200, "GET", "/api/v1/knowledge/documents/"+v1["documentId"].(string), "alice", nil); got["status"] != "active" {
		t.Fatalf("v1 status %v", got["status"])
	}
	if verify(e, "carol", p1)["stale"] != false {
		t.Fatal("a package citing the version in force is stale")
	}
	// v2 comes into force (its window is moved: no waiting)
	sqlAsOwner(t, e, `UPDATE knowledge_documents SET valid_from = now() - interval '1 minute' WHERE id = $1`, v2["documentId"])
	p2 := resolveKnowledge(e, "carol", "tariffcard", nil)
	if has(p2, "oldtariff") || !has(p2, "newtariff") {
		t.Fatalf("after v2 is in force: %v", items(p2))
	}
	if ver := verify(e, "carol", p1); ver["stale"] != true || fmt.Sprint(ver["items"]) == "" {
		t.Fatalf("a package citing the replaced version verifies as not stale: %v", ver)
	}
	if verify(e, "carol", p2)["stale"] != false {
		t.Fatal("the package citing the version in force is stale")
	}
	if got := e.must(200, "GET", "/api/v1/knowledge/documents/"+v1["documentId"].(string), "alice", nil); got["status"] != "superseded" {
		t.Fatalf("the replaced version shows %v in the admin list", got["status"])
	}
	// the same view by time: before v2's window v1 is the one
	before := resolveKnowledge(e, "carol", "tariffcard", map[string]any{"asOf": map[string]any{"valid": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}})
	if !has(before, "oldtariff") {
		t.Fatalf("as of before v2's window: %v", items(before))
	}
}

// D52: revoking the version in force makes the previous one current again, and every view agrees: retrieval, the
// bitemporal view of now, the package check. (The reviewer's probe: as-of now returned the old version while the
// current view returned nothing.)
func TestRevokedSuccessorLeavesOneAnswer(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	body := func(text string) map[string]any { return kbDoc("handbook", "handbook://fee", "", text) }
	e.must(201, "POST", "/api/v1/knowledge/documents", "alice", body("feecard oldfee"))
	v2 := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", body("feecard newfee"))
	pNew := resolveKnowledge(e, "carol", "feecard", nil)
	if !has(pNew, "newfee") || has(pNew, "oldfee") {
		t.Fatalf("v2 current: %v", items(pNew))
	}
	e.must(200, "DELETE", "/api/v1/knowledge/documents/"+v2["documentId"].(string)+"?reason=wrong", "alice", nil)
	now := resolveKnowledge(e, "carol", "feecard", nil)
	asOfNow := resolveKnowledge(e, "carol", "feecard", map[string]any{"asOf": map[string]any{"valid": e.now.UTC().Format(time.RFC3339)}})
	for name, p := range map[string]map[string]any{"current": now, "as-of now": asOfNow} {
		if !has(p, "oldfee") || has(p, "newfee") {
			t.Fatalf("%s after the revocation: %v", name, items(p))
		}
	}
	if verify(e, "carol", pNew)["stale"] != true {
		t.Fatal("a package citing the revoked version is not stale")
	}
	if verify(e, "carol", now)["stale"] != false {
		t.Fatal("the package citing the version that is current again is stale")
	}
	// a bitemporal view from before the revocation still shows what was known then
	past := resolveKnowledge(e, "carol", "feecard", map[string]any{"asOf": map[string]any{"valid": e.now.UTC().Format(time.RFC3339), "recorded": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}})
	if len(items(past)) != 0 {
		t.Fatalf("recorded before either version existed: %v", items(past))
	}
}

// S6: the other ways a cited version stops being current.
func TestPackageStaleness(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	e.must(201, "POST", "/api/v1/knowledge/bases", "alice", kbBase("arch", "tenant", "", "handbook", nil, 0, 0))
	cases := map[string]func(id string){
		"revoked": func(id string) { e.must(200, "DELETE", "/api/v1/knowledge/documents/"+id+"?reason=x", "alice", nil) },
		"expired by its window (not yet swept)": func(id string) {
			sqlAsOwner(t, e, `UPDATE knowledge_documents SET valid_until = now() - interval '1 minute' WHERE id = $1`, id)
		},
		"its base archived": func(string) { e.must(200, "POST", "/api/v1/knowledge/bases/arch/archive", "alice", nil) },
	}
	n := 0
	for name, change := range cases {
		n++
		word := fmt.Sprintf("stalequartz%d", n)
		doc := kbDoc("handbook", fmt.Sprintf("kb://arch/s%d", n), "arch", word)
		if name != "its base archived" {
			doc = kbDoc("handbook", fmt.Sprintf("handbook://stale%d", n), "", word)
		}
		d := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", doc)
		p := resolveKnowledge(e, "carol", word, nil)
		if !has(p, word) || verify(e, "carol", p)["stale"] != false {
			t.Fatalf("%s: not fresh to begin with", name)
		}
		change(d["documentId"].(string))
		ver := verify(e, "carol", p)
		got := e.must(200, "GET", "/v1/context/packages/"+p["packageId"].(string), "carol", nil)
		if ver["stale"] != true || got["stale"] != true {
			t.Fatalf("%s: verify stale=%v, get stale=%v", name, ver["stale"], got["stale"])
		}
		if has(resolveKnowledge(e, "carol", word, nil), word) {
			t.Fatalf("%s: still retrieved", name)
		}
	}
}

// S7: a document written without validFrom is valid from the beginning: its bitemporal view is what it was before
// validFrom existed - valid at any time, known from its registration on.
func TestAsOfOfADocumentWithoutValidFrom(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://old", "", "agelesscard nowindow"))
	long := time.Now().AddDate(-5, 0, 0).UTC().Format(time.RFC3339)
	if p := resolveKnowledge(e, "carol", "agelesscard", map[string]any{"asOf": map[string]any{"valid": long}}); !has(p, "nowindow") {
		t.Fatalf("valid five years ago: %v", items(p))
	}
	// known: not before it was registered
	if p := resolveKnowledge(e, "carol", "agelesscard", map[string]any{"asOf": map[string]any{"valid": long, "recorded": long}}); has(p, "nowindow") {
		t.Fatalf("recorded five years ago: %v", items(p))
	}
	// a document that does name validFrom is not valid before it
	d := kbDoc("handbook", "handbook://dated", "", "datedcard windowed")
	d["validFrom"] = time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	e.must(201, "POST", "/api/v1/knowledge/documents", "alice", d)
	if p := resolveKnowledge(e, "carol", "datedcard", map[string]any{"asOf": map[string]any{"valid": time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)}}); has(p, "windowed") {
		t.Fatalf("valid before its validFrom: %v", items(p))
	}
}
