package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"idp-cmi/migrations"
)

// Thread isolation (ADR-0001 D44, security review O1): a thread id is in runs, packages and logs, so naming a thread is not
// enough to read its memory. Written without an ACL, a thread's memory is its writer's and the administrators'.
func TestThreadMemoryIsItsWritersUnlessShared(t *testing.T) {
	e := newEnv(t)
	e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "assistant", "displayName": "A", "kind": "agent", "trust": "informational"})
	thread := map[string]any{"kind": "thread", "id": "thr-own"}
	rec := e.must(201, "POST", "/v1/memory/records", "bob", memory("assistant", "d1", "thread", "thr-own", "The decision for execution thr-own approved a refund of 40 EUR", "internal", e.now))
	id := rec["recordId"].(string)
	acl := rec["acl"].(map[string]any)
	if fmt.Sprint(acl["subjects"]) != "[bob]" || fmt.Sprint(acl["roles"]) != "[cmi-admin]" {
		t.Fatalf("a thread record written without an ACL: %v", acl)
	}
	resolve := func(who string, body map[string]any) string {
		return fmt.Sprint(items(e.must(200, "POST", "/v1/context/resolve", who, body)))
	}
	listed := func(who string) int {
		return len(e.must(200, "GET", "/v1/memory/records?scopeKind=thread&scopeId=thr-own", who, nil)["records"].([]any))
	}

	t.Run("a-reader-who-names-the-thread-sees-nothing", func(t *testing.T) {
		for _, body := range []map[string]any{
			{"query": "refund decision execution", "purpose": "t", "scope": thread},
			{"query": "refund decision execution", "purpose": "t", "scopes": []map[string]any{thread}},
		} {
			if got := resolve("carol", body); strings.Contains(got, "refund of 40 EUR") {
				t.Fatalf("carol named the thread and read it: %s", got)
			}
		}
		if n := listed("carol"); n != 0 {
			t.Fatalf("carol lists %d records of the thread", n)
		}
		e.must(404, "GET", "/v1/memory/records/"+id, "carol", nil)
		if got := fmt.Sprint(e.must(200, "GET", "/v1/memory/records?limit=200", "dan", nil)); strings.Contains(got, id) {
			t.Fatal("dan lists the thread's record")
		}
	})

	t.Run("its-writer-and-administrators-read-it", func(t *testing.T) {
		if got := resolve("bob", map[string]any{"query": "refund decision execution", "purpose": "t", "scope": thread}); !strings.Contains(got, "refund of 40 EUR") {
			t.Fatalf("the writer cannot read its thread: %s", got)
		}
		if n := listed("bob"); n != 1 {
			t.Fatalf("bob lists %d", n)
		}
		if n := listed("alice"); n != 1 {
			t.Fatalf("alice lists %d", n)
		}
		e.must(200, "GET", "/v1/memory/records/"+id, "alice", nil)
	})

	t.Run("a-writer-that-names-its-readers-shares-it", func(t *testing.T) {
		m := memory("assistant", "d2", "thread", "thr-own", "In thread thr-own the customer asked for an invoice copy", "internal", e.now)
		m["acl"] = map[string]any{"subjects": []string{"carol"}}
		shared := e.must(201, "POST", "/v1/memory/records", "bob", m)
		if fmt.Sprint(shared["acl"].(map[string]any)["subjects"]) != "[carol]" {
			t.Fatalf("an explicit ACL was replaced: %v", shared["acl"])
		}
		got := resolve("carol", map[string]any{"query": "invoice copy refund", "purpose": "t", "scope": thread})
		if !strings.Contains(got, "invoice copy") || strings.Contains(got, "refund of 40 EUR") {
			t.Fatalf("carol reads exactly what was shared with her: %s", got)
		}
	})

	t.Run("other-scopes-are-unchanged", func(t *testing.T) {
		rec := e.must(201, "POST", "/v1/memory/records", "bob", memory("assistant", "t1", "tenant", "", "Depot Nord closes at 18:00 on Fridays", "internal", e.now))
		size := func(v any) int { l, _ := v.([]any); return len(l) } // empty lists are omitted
		if a, _ := rec["acl"].(map[string]any); size(a["subjects"]) != 0 || size(a["roles"]) != 0 {
			t.Fatalf("a tenant record got an ACL: %v", a)
		}
		if got := resolve("carol", map[string]any{"query": "Depot Nord Fridays", "purpose": "t"}); !strings.Contains(got, "18:00") {
			t.Fatalf("tenant memory is shared: %s", got)
		}
	})
}

// Migration 0012 gives the thread records written before D44 the same ACL (the thread's writers and the administrators)
// and redacts connector headers that look like credentials (D45). Its effect is reproduced here on rows made to look
// like the old ones; the migration only touches such rows, so running it again is safe.
func TestMigration0012NarrowsOldThreadsAndRedactsHeaders(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "assistant", "displayName": "A", "kind": "agent", "trust": "informational"})
	e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "docs", "displayName": "Docs", "kind": "document-store", "trust": "authoritative"})
	docs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, `[]`) }))
	defer docs.Close()
	var ids []string
	for i, who := range []string{"bob", "bob", "alice"} {
		rec := e.must(201, "POST", "/v1/memory/records", who, memory("assistant", fmt.Sprintf("old%d", i), "thread", "thr-old", fmt.Sprintf("Old thread note %d about a customs hold", i), "internal", e.now))
		ids = append(ids, rec["recordId"].(string))
	}
	other := e.must(201, "POST", "/v1/memory/records", "bob", memory("assistant", "old-other", "thread", "thr-other", "Old note in another thread about a customs hold", "internal", e.now))["recordId"].(string)
	con := e.must(201, "POST", "/api/v1/connectors", "alice", map[string]any{"source": "docs", "name": "site", "kind": "http-manifest",
		"config": map[string]any{"url": docs.URL + "/m.json", "baseUrl": docs.URL, "sensitivity": "internal", "acl": map[string]any{}, "headers": []string{"Accept-Language: en"}}})["connectorId"].(string)

	conn, err := pgx.Connect(ctx, e.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	// what the rows looked like before: thread records without an ACL, a connector with credentials in its headers
	if _, err := conn.Exec(ctx, `UPDATE memory_records SET acl_subjects = '{}', acl_roles = '{}' WHERE scope_kind = 'thread'`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE connectors SET config = jsonb_set(config, '{headers}', '["Accept-Language: en", "Cookie: sid=s3cr3t", "X-Custom: Bearer abc.def"]') WHERE id = $1`, con); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(items(e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": "customs hold thread note", "purpose": "t", "scope": map[string]any{"kind": "thread", "id": "thr-old"}}))); !strings.Contains(got, "Old thread note") {
		t.Fatalf("the old rows are not what the migration is for: %s", got)
	}

	sql, err := migrations.FS.ReadFile("0012_thread_acl.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second run changes nothing
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	var subjects, roles []string
	for _, id := range ids {
		if err := conn.QueryRow(ctx, `SELECT acl_subjects, acl_roles FROM memory_records WHERE id = $1`, id).Scan(&subjects, &roles); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(subjects) != "[alice bob]" || fmt.Sprint(roles) != "[cmi-admin]" {
			t.Fatalf("%s: %v %v", id, subjects, roles)
		}
	}
	if err := conn.QueryRow(ctx, `SELECT acl_subjects FROM memory_records WHERE id = $1`, other).Scan(&subjects); err != nil || fmt.Sprint(subjects) != "[bob]" {
		t.Fatalf("each thread has its own writers: %v %v", subjects, err)
	}
	if got := fmt.Sprint(items(e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": "customs hold thread note", "purpose": "t", "scope": map[string]any{"kind": "thread", "id": "thr-old"}}))); strings.Contains(got, "Old thread note") {
		t.Fatalf("carol still reads the old thread: %s", got)
	}
	if got := fmt.Sprint(items(e.must(200, "POST", "/v1/context/resolve", "bob", map[string]any{"query": "customs hold thread note", "purpose": "t", "scope": map[string]any{"kind": "thread", "id": "thr-old"}}))); !strings.Contains(got, "Old thread note") {
		t.Fatalf("a writer of the thread lost it: %s", got)
	}
	var headers string
	if err := conn.QueryRow(ctx, `SELECT config->>'headers' FROM connectors WHERE id = $1`, con).Scan(&headers); err != nil {
		t.Fatal(err)
	}
	if headers != `["Accept-Language: en", "Cookie: <redacted>", "X-Custom: <redacted>"]` {
		t.Fatalf("headers after the migration: %s", headers)
	}
	var audits int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'migration.0012_thread_acl' AND tenant_id = $1`, e.tenantA).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit events: %d %v", audits, err)
	}
	// a connector with a redacted credential is not used
	if st, out := e.call("POST", "/api/v1/connectors/site/sync", "alice", nil); st != 502 || !strings.Contains(fmt.Sprint(out), "Cookie looks like a credential") {
		t.Fatalf("%d %v", st, out)
	}
}

// Connector headers may not carry credentials (ADR-0001 D45, security review O3).
func TestConnectorHeadersMayNotCarryCredentials(t *testing.T) {
	e := newEnv(t)
	e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "docs", "displayName": "Docs", "kind": "document-store", "trust": "authoritative"})
	body := func(name string, headers ...string) map[string]any {
		return map[string]any{"source": "docs", "name": name, "kind": "http-manifest",
			"config": map[string]any{"url": "https://docs.example/m.json", "baseUrl": "https://docs.example", "sensitivity": "internal", "acl": map[string]any{}, "headers": headers}}
	}
	for i, h := range []string{"Authorization: Bearer x", "Cookie: sid=1", "X-Api-Key: k", "X-Auth-Token: t", "Proxy-Authorization: Basic eDp5", "X-Custom: Bearer abc"} {
		if st, out := e.call("POST", "/api/v1/connectors", "alice", body(fmt.Sprintf("c%d", i), "Accept-Language: en", h)); st != 422 || !strings.Contains(fmt.Sprint(out), "credential") {
			t.Fatalf("%q: %d %v", h, st, out)
		}
	}
	e.must(201, "POST", "/api/v1/connectors", "alice", body("ok", "Accept-Language: en", "X-Requested-With: idp-cmi"))
}
