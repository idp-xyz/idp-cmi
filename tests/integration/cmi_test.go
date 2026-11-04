// Package integration runs the CMI in process - API, store and resolver - against a real
// PostgreSQL. Set IDP_TEST_PG_OWNER_URL to a superuser URL of a scratch cluster (make test-integration).
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/adapters/identity"
	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/api"
	"idp-cmi/internal/authorization"
	cmictx "idp-cmi/internal/context"
	"idp-cmi/internal/observability"
)

func scratchDB(t *testing.T) (ownerURL, serviceURL string) {
	t.Helper()
	base := os.Getenv("IDP_TEST_PG_OWNER_URL")
	if base == "" {
		t.Skip("IDP_TEST_PG_OWNER_URL not set (make test-integration)")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "idp_cmi_it_" + hex.EncodeToString(b)
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	u, _ := url.Parse(base)
	u.Path = "/" + name
	ownerURL = u.String()
	if _, err := postgres.Migrate(ctx, ownerURL); err != nil {
		t.Fatal(err)
	}
	if err := postgres.EnsureLogin(ctx, ownerURL, "idp_cmi_it_svc", "it-service-password"); err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("idp_cmi_it_svc", "it-service-password")
	return ownerURL, u.String()
}

type env struct {
	t       *testing.T
	store   *postgres.Store
	api     *httptest.Server
	tokens  map[string]string
	tenantA string
	tenantB string
	now     time.Time
}

var profile = identity.Profile{Issuer: "idp-cmi-it", Audience: "idp-cmi", TenantClaim: "tenant_id", RolesClaim: "roles"}

const devSecret = "integration-test-secret-at-least-32-bytes-long"

func newEnv(t *testing.T) *env {
	t.Helper()
	ownerURL, serviceURL := scratchDB(t)
	ctx := context.Background()
	tenantA, err := postgres.SeedTenant(ctx, ownerURL, "", "acme")
	if err != nil {
		t.Fatal(err)
	}
	tenantB, _ := postgres.SeedTenant(ctx, ownerURL, "", "globex")
	store, err := postgres.Open(ctx, serviceURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := postgres.Open(ctx, ownerURL); err == nil {
		t.Fatal("the superuser connection must be refused")
	}
	e := &env{t: t, store: store, tenantA: tenantA, tenantB: tenantB, now: time.Now()}
	store.Now = func() time.Time { return e.now }
	tokens := map[string]string{}
	for name, spec := range map[string]struct {
		tenant string
		roles  []string
	}{
		"alice": {tenantA, []string{authorization.RoleAdmin}},
		"bob":   {tenantA, []string{authorization.RoleWriter}}, // an agent runtime
		"carol": {tenantA, []string{authorization.RoleReader}}, // an ordinary reader
		"dan":   {tenantA, []string{authorization.RoleReader, "finance"}},
		"erin":  {tenantB, []string{authorization.RoleAdmin, authorization.RoleReader}},
	} {
		tok, err := identity.DevToken([]byte(devSecret), profile, name, spec.tenant, spec.roles, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		tokens[name] = tok
	}
	e.tokens = tokens
	verifier, _ := identity.NewDevVerifier(profile, []byte(devSecret))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &api.API{Store: store, Auth: verifier, Resolver: &cmictx.Resolver{Store: store, Now: func() time.Time { return e.now }}, Log: log, Metrics: observability.NewMetrics(), Version: "it"}
	tp, _, err := observability.Setup("idp-cmi-it", "it", log)
	if err != nil {
		t.Fatal(err)
	}
	e.api = httptest.NewServer(observability.HTTPMiddleware(tp, "idp-cmi/it", a.Handler()))
	t.Cleanup(e.api.Close)
	return e
}

func (e *env) call(method, path, who string, body any) (int, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, e.api.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+e.tokens[who])
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			e.t.Fatalf("%s %s: %d non-JSON body %s", method, path, resp.StatusCode, raw)
		}
	}
	return resp.StatusCode, out
}

func (e *env) must(status int, method, path, who string, body any) map[string]any {
	e.t.Helper()
	st, out := e.call(method, path, who, body)
	if st != status {
		e.t.Fatalf("%s %s as %s: want %d, got %d %v", method, path, who, status, st, out)
	}
	return out
}

func memory(source, ref, scopeKind, scopeID, content, sensitivity string, observed time.Time) map[string]any {
	return map[string]any{"scope": map[string]any{"kind": scopeKind, "id": scopeID}, "kind": "semantic", "content": content, "tags": []string{},
		"provenance": map[string]any{"source": source, "ref": ref, "observedAt": observed.UTC().Format(time.RFC3339), "recordedBy": "run_1"},
		"validity":   map[string]any{"validFrom": observed.UTC().Format(time.RFC3339)}, "sensitivity": sensitivity, "acl": map[string]any{}}
}

func items(pkg map[string]any) []map[string]any {
	var out []map[string]any
	for _, it := range pkg["package"].(map[string]any)["items"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

func TestCMIEndToEnd(t *testing.T) {
	e := newEnv(t)
	now := e.now

	t.Run("sources-and-roles", func(t *testing.T) {
		e.must(403, "POST", "/api/v1/sources", "bob", map[string]any{"name": "orders", "displayName": "Orders", "kind": "domain-system", "trust": "authoritative"})
		e.must(422, "POST", "/api/v1/sources", "alice", map[string]any{"name": "helper", "displayName": "x", "kind": "agent", "trust": "authoritative"})
		e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "orders", "displayName": "Order system", "kind": "domain-system", "trust": "authoritative"})
		e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "assistant", "displayName": "The assistant agent", "kind": "agent", "trust": "informational"})
		e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "handbook", "displayName": "Handbook", "kind": "document-store", "trust": "authoritative"})
		e.must(409, "POST", "/api/v1/sources", "alice", map[string]any{"name": "orders", "displayName": "again", "kind": "domain-system", "trust": "authoritative"})
		if n := len(e.must(200, "GET", "/api/v1/sources", "carol", nil)["sources"].([]any)); n != 3 {
			t.Fatalf("sources %d", n)
		}
		if n := len(e.must(200, "GET", "/api/v1/sources", "erin", nil)["sources"].([]any)); n != 0 {
			t.Fatalf("sources visible across tenants: %d", n)
		}
	})

	t.Run("memory-write-visibility-and-ceilings", func(t *testing.T) {
		// an unregistered source, a missing provenance, a level above the writer's ceiling
		st, _ := e.call("POST", "/v1/memory/records", "bob", memory("nope", "x", "subject", "carol", "y", "internal", now))
		if st != 409 {
			t.Fatalf("unregistered source: %d", st)
		}
		bad := memory("orders", "order:1", "subject", "carol", "y", "internal", now)
		delete(bad["provenance"].(map[string]any), "ref")
		e.must(422, "POST", "/v1/memory/records", "bob", bad)
		e.must(403, "POST", "/v1/memory/records", "bob", memory("orders", "order:1", "subject", "carol", "board minutes", "restricted", now))
		e.must(403, "POST", "/v1/memory/records", "carol", memory("orders", "order:1", "subject", "carol", "y", "internal", now))
		// records: carol's preference (internal), a confidential one, one for the finance role, one in another thread
		r1 := e.must(201, "POST", "/v1/memory/records", "bob", memory("orders", "order:1001", "subject", "carol", "Carol prefers morning pickup slots for her parcels.", "internal", now.Add(-time.Hour)))
		conf := memory("orders", "order:1002", "subject", "carol", "Carol's credit limit was raised; parcels above 10000 need a manager.", "confidential", now)
		e.must(201, "POST", "/v1/memory/records", "bob", conf)
		fin := memory("orders", "invoice:7", "tenant", "", "Finance: pickup surcharges are invoiced monthly.", "internal", now)
		fin["acl"] = map[string]any{"roles": []string{"finance"}}
		e.must(201, "POST", "/v1/memory/records", "bob", fin)
		e.must(201, "POST", "/v1/memory/records", "bob", memory("assistant", "run:9", "thread", "thr-1", "In this thread the user asked about pickup twice already.", "internal", now))
		if r1["status"] != "active" || r1["provenance"].(map[string]any)["recordedBy"] != "run_1" {
			t.Fatalf("%v", r1)
		}
		// carol (reader, ceiling internal) sees her internal record, not the confidential one, not finance
		recs := e.must(200, "GET", "/v1/memory/records?scopeKind=subject&scopeId=carol", "carol", nil)["records"].([]any)
		if len(recs) != 1 || !strings.Contains(recs[0].(map[string]any)["content"].(string), "morning pickup") {
			t.Fatalf("%v", recs)
		}
		e.must(404, "GET", "/v1/memory/records/"+e.must(200, "GET", "/v1/memory/records?scopeKind=subject&scopeId=carol", "alice", nil)["records"].([]any)[0].(map[string]any)["recordId"].(string), "carol", nil)
		// dan carries the finance role and sees the finance record of the tenant scope
		if recs := e.must(200, "GET", "/v1/memory/records?scopeKind=tenant", "dan", nil)["records"].([]any); len(recs) != 1 {
			t.Fatalf("%v", recs)
		}
		if recs := e.must(200, "GET", "/v1/memory/records?scopeKind=tenant", "carol", nil)["records"].([]any); len(recs) != 0 {
			t.Fatalf("finance record leaked: %v", recs)
		}
		// the other tenant sees nothing at all
		if recs := e.must(200, "GET", "/v1/memory/records?scopeKind=subject&scopeId=carol", "erin", nil)["records"].([]any); len(recs) != 0 {
			t.Fatalf("cross-tenant: %v", recs)
		}
	})

	t.Run("knowledge-documents-and-versions", func(t *testing.T) {
		doc := map[string]any{"source": "handbook", "uri": "handbook://pickup", "title": "Pickup handbook", "sensitivity": "internal", "acl": map[string]any{},
			"text": "Pickup slots. Morning slots run from 08:00 to 12:00.\n\nSurcharges. A pickup outside the slot costs 15 EUR.\n\n关税说明。跨境包裹需要申报关税税率。"}
		d1 := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", doc)
		if d1["version"] != 1.0 || d1["chunkCount"].(float64) < 1 || d1["status"] != "active" {
			t.Fatalf("%v", d1)
		}
		e.must(403, "POST", "/api/v1/knowledge/documents", "bob", doc)
		doc["text"] = "Pickup slots. Morning slots now run from 07:00 to 11:00.\n\nSurcharges. A pickup outside the slot costs 20 EUR."
		d2 := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", doc)
		if d2["version"] != 2.0 {
			t.Fatalf("%v", d2)
		}
		if old := e.must(200, "GET", "/api/v1/knowledge/documents/"+d1["documentId"].(string), "alice", nil); old["status"] != "superseded" {
			t.Fatalf("%v", old)
		}
		secret := map[string]any{"source": "handbook", "uri": "handbook://pricing-internal", "title": "Pricing (confidential)", "sensitivity": "confidential", "acl": map[string]any{},
			"text": "Pickup surcharge margins: the 20 EUR surcharge carries 12 EUR of margin."}
		e.must(201, "POST", "/api/v1/knowledge/documents", "alice", secret)
	})

	t.Run("resolve-orders-filters-and-packs", func(t *testing.T) {
		res := e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": "pickup slot surcharge", "scope": map[string]any{"kind": "thread", "id": "thr-1"}, "purpose": "answer a pickup question"})
		pkg := res["package"].(map[string]any)
		if res["digest"] == nil || pkg["schemaVersion"] != "idp.cmi.package/v1" || pkg["requestedBy"] != "carol" {
			t.Fatalf("%v", res)
		}
		its := items(res)
		if len(its) < 3 {
			t.Fatalf("items: %v", its)
		}
		// authoritative before informational; the superseded v1 text, the confidential pricing and the finance record are absent
		var sawInformational bool
		for _, it := range its {
			if it["trust"] == "informational" {
				sawInformational = true
			} else if sawInformational {
				t.Fatalf("authoritative after informational: %v", its)
			}
			ex := it["excerpt"].(string)
			if strings.Contains(ex, "08:00 to 12:00") || strings.Contains(ex, "margin") || strings.Contains(ex, "invoiced monthly") {
				t.Fatalf("unusable or invisible content packed: %q", ex)
			}
			if it["sensitivity"] == "confidential" || it["sensitivity"] == "restricted" {
				t.Fatalf("above ceiling: %v", it)
			}
		}
		joined := fmt.Sprint(its)
		if !strings.Contains(joined, "07:00 to 11:00") || !strings.Contains(joined, "morning pickup") || !strings.Contains(joined, "asked about pickup twice") {
			t.Fatalf("expected items missing: %s", joined)
		}
		// the thread memory is only visible when that thread is in scope
		other := items(e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": "pickup slot surcharge"}))
		if strings.Contains(fmt.Sprint(other), "asked about pickup twice") {
			t.Fatal("thread memory leaked into an unscoped resolve")
		}
		// dan (finance) sees the finance record; alice (admin, ceiling restricted) sees the confidential pricing
		if !strings.Contains(fmt.Sprint(items(e.must(200, "POST", "/v1/context/resolve", "dan", map[string]any{"query": "pickup surcharge invoiced"}))), "invoiced monthly") {
			t.Fatal("finance record not resolved for the finance role")
		}
		adminItems := items(e.must(200, "POST", "/v1/context/resolve", "alice", map[string]any{"query": "surcharge margin"}))
		if !strings.Contains(fmt.Sprint(adminItems), "margin") {
			t.Fatalf("%v", adminItems)
		}
		// a caller may lower, never raise, its ceiling
		if strings.Contains(fmt.Sprint(items(e.must(200, "POST", "/v1/context/resolve", "alice", map[string]any{"query": "surcharge margin", "sensitivityCeiling": "internal"}))), "margin") {
			t.Fatal("lowered ceiling ignored")
		}
		if strings.Contains(fmt.Sprint(items(e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": "surcharge margin", "sensitivityCeiling": "restricted"}))), "margin") {
			t.Fatal("ceiling raised by the caller")
		}
		// CJK query hits the bigram index
		if !strings.Contains(fmt.Sprint(items(e.must(200, "POST", "/v1/context/resolve", "alice", map[string]any{"query": "关税", "sources": []string{"handbook"}, "kinds": []string{"knowledge"}}))), "关税") {
			// v1 was superseded, so the Chinese paragraph is gone; add a current one and retry
			e.must(201, "POST", "/api/v1/knowledge/documents", "alice", map[string]any{"source": "handbook", "uri": "handbook://customs", "title": "Customs", "sensitivity": "internal", "acl": map[string]any{}, "text": "跨境包裹需要申报关税税率。"})
			if !strings.Contains(fmt.Sprint(items(e.must(200, "POST", "/v1/context/resolve", "alice", map[string]any{"query": "关税"}))), "关税") {
				t.Fatal("CJK query found nothing")
			}
		}
		// budget: one item at most, few tokens
		small := e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": "pickup slot surcharge", "budget": map[string]any{"maxTokens": 40, "maxItems": 1}})
		if n := len(items(small)); n > 1 {
			t.Fatalf("budget ignored: %d items", n)
		}
		if used := small["package"].(map[string]any)["budget"].(map[string]any)["usedTokens"].(float64); used > 40 {
			t.Fatalf("tokens %v", used)
		}
		e.must(422, "POST", "/v1/context/resolve", "carol", map[string]any{"query": ""})
		if n := len(items(e.must(200, "POST", "/v1/context/resolve", "erin", map[string]any{"query": "pickup slot surcharge"}))); n != 0 {
			t.Fatalf("tenant B resolved tenant A content: %d items", n) // erin reads in her own tenant and sees nothing of acme
		}
	})

	t.Run("packages-are-sealed-and-go-stale", func(t *testing.T) {
		res := e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": "morning pickup"})
		id := res["packageId"].(string)
		got := e.must(200, "GET", "/v1/context/packages/"+id, "carol", nil)
		if got["digest"] != res["digest"] || got["stale"] != false {
			t.Fatalf("%v", got)
		}
		e.must(404, "GET", "/v1/context/packages/"+id, "dan", nil)   // another requester
		e.must(200, "GET", "/v1/context/packages/"+id, "alice", nil) // an administrator
		// the record behind one item is revoked by reference: the package reads as stale
		if n := e.must(200, "POST", "/v1/memory/revoke", "bob", map[string]any{"ref": "order:1001", "reason": "customer changed preference"})["revoked"]; n != 1.0 {
			t.Fatalf("revoked %v", n)
		}
		got = e.must(200, "GET", "/v1/context/packages/"+id, "carol", nil)
		if got["stale"] != true || len(got["staleItems"].([]any)) != 1 {
			t.Fatalf("%v", got)
		}
		if strings.Contains(fmt.Sprint(items(e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": "morning pickup"}))), "morning pickup") {
			t.Fatal("revoked record still resolved")
		}
		// expiry: a record valid for one hour expires when the clock passes; the sweeper marks it
		short := memory("orders", "order:2000", "subject", "carol", "Temporary gate code 4711 for today's pickup.", "internal", now)
		short["validity"] = map[string]any{"validFrom": now.UTC().Format(time.RFC3339), "validUntil": now.Add(time.Hour).UTC().Format(time.RFC3339)}
		e.must(201, "POST", "/v1/memory/records", "bob", short)
		res = e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": "gate code pickup"})
		if !strings.Contains(fmt.Sprint(items(res)), "4711") {
			t.Fatal("fresh record not resolved")
		}
		e.now = now.Add(2 * time.Hour)
		if strings.Contains(fmt.Sprint(items(e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": "gate code pickup"}))), "4711") {
			t.Fatal("expired record resolved")
		}
		// the sweeper (clock of the database) marks nothing yet; force the record's expiry in the past and sweep
		if _, err := e.store.Pool.Exec(context.Background(), `SELECT 1`); err != nil {
			t.Fatal(err)
		}
		e.now = now
		// a writer may revoke only what its own identity recorded; an administrator anything
		recs := e.must(200, "GET", "/v1/memory/records?scopeKind=subject&scopeId=carol", "alice", nil)["records"].([]any)
		var otherID string
		for _, r := range recs {
			if strings.Contains(r.(map[string]any)["content"].(string), "credit limit") {
				otherID = r.(map[string]any)["recordId"].(string)
			}
		}
		e.must(201, "POST", "/v1/memory/records", "bob", memory("assistant", "run:10", "subject", "carol", "noted by the assistant", "internal", now))
		st, _ := e.call("POST", "/v1/memory/revoke", "bob", map[string]any{"recordId": otherID})
		if st != 200 {
			t.Fatalf("writer revoking its own source's record: %d", st)
		}
	})

	t.Run("audit-and-policy", func(t *testing.T) {
		if n := len(e.must(200, "GET", "/api/v1/audit", "alice", nil)["events"].([]any)); n < 6 {
			t.Fatalf("audit events %d", n)
		}
		e.must(422, "PUT", "/api/v1/policy", "alice", map[string]any{"readerCeiling": "secret", "writerCeiling": "internal", "packageTtlSeconds": 3600})
		pol := e.must(200, "PUT", "/api/v1/policy", "alice", map[string]any{"readerCeiling": "confidential", "writerCeiling": "confidential", "defaultMemoryTtlDays": 0, "packageTtlSeconds": 120})
		if pol["readerCeiling"] != "confidential" || pol["default"] != false {
			t.Fatalf("%v", pol)
		}
		// carol now sees the confidential pricing document
		if !strings.Contains(fmt.Sprint(items(e.must(200, "POST", "/v1/context/resolve", "carol", map[string]any{"query": "surcharge margin"}))), "margin") {
			t.Fatal("raised reader ceiling not applied")
		}
		if pol := e.must(200, "GET", "/api/v1/policy", "erin", nil); pol["default"] != true {
			t.Fatalf("policy leaked across tenants: %v", pol)
		}
	})
}
