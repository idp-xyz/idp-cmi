package integration

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/adapters/blob"
)

// burst runs n calls at once and returns their statuses and bodies.
func burst(n int, call func(i int) (int, map[string]any)) ([]int, []map[string]any) {
	st := make([]int, n)
	out := make([]map[string]any, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			st[i], out[i] = call(i)
		}()
	}
	close(start)
	wg.Wait()
	return st, out
}

func baseCount(t *testing.T, e *env, where string) int {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), e.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var n int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM knowledge_bases WHERE `+where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// S4: the first use of a base is a race that nobody loses: concurrent first uploads of a member, concurrent
// bulk uploads of an administrator into a source's fresh base, and concurrent versions of one document.
func TestConcurrentFirstUseNeverFails(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	for round, who := range []string{"bob", "carol", "dan"} {
		st, out := burst(8, func(i int) (int, map[string]any) {
			return e.call("POST", "/v1/knowledge/documents", who, kbDoc("member-notes", fmt.Sprintf("kb://me/n%d", i), "", "note body"))
		})
		for i, s := range st {
			if s != 201 {
				t.Errorf("%s first uploads #%d: %d %v", who, i, s, out[i])
			}
		}
		if n := baseCount(t, e, fmt.Sprintf("scope_kind = 'subject' AND scope_id = '%s'", who)); n != 1 {
			t.Errorf("%s has %d personal bases after %d rounds", who, n, round+1)
		}
	}
	st, out := burst(8, func(i int) (int, map[string]any) {
		return e.call("POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", fmt.Sprintf("handbook://p%d", i), "", "page body"))
	})
	for i, s := range st {
		if s != 201 {
			t.Errorf("bulk admin upload #%d: %d %v", i, s, out[i])
		}
	}
	if n := baseCount(t, e, `name = 'handbook'`); n != 1 {
		t.Errorf("%d bases for the source", n)
	}
	// ten versions of one uri at once: all stored, numbered 1..10, exactly one current
	st, out = burst(10, func(i int) (int, map[string]any) {
		return e.call("POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://same", "", fmt.Sprintf("sameword%d", i)))
	})
	var versions []int
	for i, s := range st {
		if s != 201 {
			t.Fatalf("version #%d: %d %v", i, s, out[i])
		}
		versions = append(versions, int(out[i]["version"].(float64)))
	}
	sort.Ints(versions)
	for i, v := range versions {
		if v != i+1 {
			t.Fatalf("versions %v", versions)
		}
	}
	seen := 0
	for i := 0; i < 10; i++ {
		if kbSees(e, "carol", fmt.Sprintf("sameword%d", i)) {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("%d versions of one uri are current", seen)
	}
}

// S8: the quota check and the insert are one step: a burst cannot overshoot the base's limit.
func TestQuotaHoldsUnderConcurrency(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	e.must(201, "POST", "/api/v1/knowledge/bases", "alice", kbBase("small", "tenant", "", "handbook", nil, 3, 0))
	st, _ := burst(12, func(i int) (int, map[string]any) {
		return e.call("POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", fmt.Sprintf("kb://small/d%d", i), "small", "quota body"))
	})
	ok, full := 0, 0
	for _, s := range st {
		switch s {
		case 201:
			ok++
		case 429:
			full++
		}
	}
	if ok != 3 || full != 9 {
		t.Fatalf("statuses %v", st)
	}
	// the bytes limit too
	e.must(201, "POST", "/api/v1/knowledge/bases", "alice", kbBase("bytes", "tenant", "", "handbook", nil, 0, 1000))
	st, _ = burst(10, func(i int) (int, map[string]any) {
		return e.call("POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", fmt.Sprintf("kb://bytes/d%d", i), "bytes", strings.Repeat("x", 300)))
	})
	ok = 0
	for _, s := range st {
		if s == 201 {
			ok++
		}
	}
	if ok != 3 {
		t.Fatalf("%d uploads of 300 bytes fit a 1000 byte limit: %v", ok, st)
	}
}

// S8: what counts against a quota.
func TestQuotaCounting(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	e.store.Originals = &blob.Mem{}
	post := func(who, route, uri, base, text string, from time.Time) (int, map[string]any) {
		b := kbDoc("handbook", uri, base, text)
		if who == "bob" {
			b["source"] = "member-notes"
		}
		if !from.IsZero() {
			b["validFrom"] = from.UTC().Format(time.RFC3339)
		}
		return e.call("POST", route, who, b)
	}

	// a full base takes a new version of a document it already holds
	e.must(201, "POST", "/api/v1/knowledge/bases", "alice", kbBase("one", "tenant", "", "handbook", nil, 1, 0))
	if st, out := post("alice", "/api/v1/knowledge/documents", "kb://one/only", "one", "first version", time.Time{}); st != 201 {
		t.Fatal(st, out)
	}
	if st, out := post("alice", "/api/v1/knowledge/documents", "kb://one/only", "one", "second version", time.Time{}); st != 201 {
		t.Fatalf("a new version of the one document of a full base: %d %v", st, out)
	}
	st, out := post("alice", "/api/v1/knowledge/documents", "kb://one/other", "one", "another document", time.Time{})
	mustFail(t, st, out, 429, "QUOTA_EXCEEDED")

	// the replaced version's bytes are freed: three versions in force, one at a time, fit a base that holds one
	e.must(201, "POST", "/api/v1/knowledge/bases", "alice", kbBase("bytes", "tenant", "", "handbook", nil, 0, 300))
	text := strings.Repeat("y", 200)
	for i := 0; i < 3; i++ {
		if st, out := post("alice", "/api/v1/knowledge/documents", "kb://bytes/doc", "bytes", text+fmt.Sprint(i), time.Time{}); st != 201 {
			t.Fatalf("version %d in force: %d %v", i, st, out)
		}
	}
	// a version that is not in force yet is held in addition (it cannot be used to get around the limit)
	st, out = post("alice", "/api/v1/knowledge/documents", "kb://bytes/doc", "bytes", text+"future", time.Now().Add(time.Hour))
	mustFail(t, st, out, 429, "QUOTA_EXCEEDED")
	e.must(201, "POST", "/api/v1/knowledge/bases", "alice", kbBase("waiting", "tenant", "", "handbook", nil, 0, 450))
	post("alice", "/api/v1/knowledge/documents", "kb://waiting/doc", "waiting", text+"a", time.Time{})
	if st, out := post("alice", "/api/v1/knowledge/documents", "kb://waiting/doc", "waiting", text+"future", time.Now().Add(time.Hour)); st != 201 {
		t.Fatalf("one version waiting for its date fits: %d %v", st, out)
	}
	st, out = post("alice", "/api/v1/knowledge/documents", "kb://waiting/doc", "waiting", text+"future2", time.Now().Add(2*time.Hour))
	mustFail(t, st, out, 429, "QUOTA_EXCEEDED")

	// a personal base without a quota of its own has the member default; originals count toward bytes
	e.store.Knowledge.PersonalQuotaDocuments = 2
	e.store.Knowledge.PersonalQuotaBytes = 3000
	post2 := func(uri, text string) (int, map[string]any) {
		return post("bob", "/v1/knowledge/documents", uri, "", text, time.Time{})
	}
	a := e.must(201, "POST", "/v1/knowledge/documents", "bob", kbDoc("member-notes", "kb://me/a", "", "first"))
	if st, out := post2("kb://me/b", "second"); st != 201 {
		t.Fatal(st, out)
	}
	st, out = post2("kb://me/c", "third")
	mustFail(t, st, out, 429, "QUOTA_EXCEEDED")
	if st, out := post2("kb://me/a", "first again"); st != 201 {
		t.Fatalf("a new version in a personal base at its document limit: %d %v", st, out)
	}
	id := a["documentId"].(string)
	// the first version was replaced; its original is not counted, the new version's is
	if st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "application/pdf", make([]byte, 100)); st != 200 {
		t.Fatalf("an original on a replaced version: %d", st)
	}
	_, mine := e.callBytesDoc(t, "bob", "kb://me/a")
	newID := mine["documentId"].(string)
	put := func(n int) int {
		st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+newID+"/original", "bob", "application/pdf", append([]byte("%PDF"), make([]byte, n)...))
		return st
	}
	if put(2500) != 200 {
		t.Fatal("an original within the bytes limit")
	}
	if st := put(3500); st != 429 {
		t.Fatalf("an original over it: %d", st)
	}
	if st := put(2900); st != 200 { // replacing counts the new one, not both
		t.Fatalf("a replacement that fits alone: %d", st)
	}
	st, out = post2("kb://me/big", strings.Repeat("z", 500))
	mustFail(t, st, out, 429, "QUOTA_EXCEEDED") // text and the original together are over
}

// callBytesDoc finds the current document of a uri in the caller's personal base through their own list.
func (e *env) callBytesDoc(t *testing.T, who, uri string) (int, map[string]any) {
	t.Helper()
	name := ownBase(e, who)
	want := strings.Replace(uri, "kb://me/", "kb://"+name+"/", 1)
	var best map[string]any
	for _, d := range e.must(200, "GET", "/v1/knowledge/documents", who, nil)["documents"].([]any) {
		m := d.(map[string]any)
		if m["uri"] == want && m["status"] == "active" {
			best = m
		}
	}
	if best == nil {
		t.Fatalf("no current document at %s", want)
	}
	return 200, best
}
