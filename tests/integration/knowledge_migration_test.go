package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/adapters/store/postgres"
)

// seedAtSchema12 puts what a release before the knowledge bases would hold: two tenants, sources, documents with
// versions, an ACL, a revocation and an old registration date.
func seedAtSchema12(t *testing.T, ownerURL string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	tenants := map[string]string{"acme": postgres.NewUUID(), "globex": postgres.NewUUID()}
	for slug, id := range tenants {
		exec(`INSERT INTO tenants (id, slug) VALUES ($1, $2)`, id, slug)
		for _, src := range [][]string{{"handbook", "document-store", "authoritative"}, {"member-notes", "user", "informational"}, {"empty-src", "document-store", "authoritative"}} {
			exec(`INSERT INTO sources (id, tenant_id, name, display_name, kind, trust, created_by) VALUES ($1, $2, $3, $3, $4, $5, 'alice')`, postgres.NewUUID(), id, src[0], src[1], src[2])
		}
	}
	doc := func(tenant, source, uri, text string, version int, status string, acl []string, ageDays int, revoked bool) {
		id := postgres.NewUUID()
		exec(`INSERT INTO knowledge_documents (id, tenant_id, source_id, uri, title, version, content_sha256, byte_size, chunk_count, status, sensitivity, acl_subjects, created_by, created_at,
			revoked_at, revoked_by, revoked_reason)
			SELECT $1, $2, s.id, $4, $4, $5, 'sha256:x', $6, 1, $7, 'internal', $8::text[], 'alice', now() - make_interval(days => $9),
			       CASE WHEN $10 THEN now() END, CASE WHEN $10 THEN 'alice' END, CASE WHEN $10 THEN 'x' END
			FROM sources s WHERE s.tenant_id = $2 AND s.name = $3`, id, tenants[tenant], source, uri, version, len(text), status, acl, ageDays, revoked)
		exec(`INSERT INTO knowledge_chunks (tenant_id, document_id, seq, content, search) VALUES ($1, $2, 0, $3, to_tsvector('simple', $3))`, tenants[tenant], id, text)
	}
	doc("acme", "handbook", "handbook://open", "openmigr everyone reads this", 1, "active", []string{}, 10, false)
	doc("acme", "handbook", "handbook://acl", "aclmigr only dan", 1, "active", []string{"dan"}, 10, false)
	doc("acme", "handbook", "handbook://ver", "vermigr oldversion", 1, "superseded", []string{}, 20, false)
	doc("acme", "handbook", "handbook://ver", "vermigr newversion", 2, "active", []string{}, 10, false)
	doc("acme", "handbook", "handbook://rev", "revmigr revoked", 1, "revoked", []string{}, 10, true)
	doc("acme", "member-notes", "notes://x", "notesmigr a user-source doc", 1, "active", []string{}, 10, false)
	doc("globex", "handbook", "handbook://open", "globexmigr other tenant", 1, "active", []string{}, 10, false)
}

// The upgrade of a populated database: every document lands in an enterprise base named after its source, nothing
// changes who sees what, and a document written without validFrom is valid from the beginning (NULL), not from
// its registration. It is run by the superuser and by a schema owner that is not one - FORCE row-level security
// applies to such an owner, and the backfill has to see the rows regardless.
func TestMigrationOnAPopulatedDatabase(t *testing.T) {
	base := os.Getenv("IDP_TEST_PG_OWNER_URL")
	if base == "" {
		t.Skip("IDP_TEST_PG_OWNER_URL not set (make test-integration)")
	}
	for _, mode := range []string{"superuser", "schema owner without superuser"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			super, err := pgx.Connect(ctx, base)
			if err != nil {
				t.Fatal(err)
			}
			defer super.Close(ctx)
			b := make([]byte, 6)
			_, _ = rand.Read(b)
			name := "idp_cmi_mig_" + hex.EncodeToString(b)
			owner := "postgres"
			if mode != "superuser" {
				owner = "idp_cmi_it_schema_owner"
				var exists bool
				_ = super.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, owner).Scan(&exists)
				if !exists {
					if _, err := super.Exec(ctx, `CREATE ROLE `+owner+` LOGIN NOSUPERUSER NOBYPASSRLS CREATEROLE PASSWORD 'owner-password'`); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := super.Exec(ctx, "CREATE DATABASE "+name+" OWNER "+owner); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				c, err := pgx.Connect(context.Background(), base)
				if err == nil {
					_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
					c.Close(context.Background())
				}
			})
			u, _ := url.Parse(base)
			u.Path = "/" + name
			superURL := u.String()
			migrator := superURL
			if mode != "superuser" {
				u.User = url.UserPassword(owner, "owner-password")
				migrator = u.String()
			}
			if _, err := postgres.MigrateUpTo(ctx, migrator, 12); err != nil {
				t.Fatal(err)
			}
			seedAtSchema12(t, superURL)
			if applied, err := postgres.Migrate(ctx, migrator); err != nil || len(applied) < 4 {
				t.Fatalf("upgrade: %v %v", applied, err)
			}
			if err := postgres.EnsureLogin(ctx, superURL, "idp_cmi_it_svc", "it-service-password"); err != nil {
				t.Fatal(err)
			}
			su, _ := url.Parse(superURL)
			su.User = url.UserPassword("idp_cmi_it_svc", "it-service-password")
			e := newEnvOn(t, superURL, su.String())

			conn, err := pgx.Connect(ctx, superURL)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(ctx)
			var nulls, validFrom, chunks, noIndex int
			if err := conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM knowledge_documents WHERE knowledge_base_id IS NULL), (SELECT count(*) FROM knowledge_documents WHERE valid_from IS NOT NULL),
				(SELECT count(*) FROM knowledge_chunks), (SELECT count(*) FROM knowledge_chunks WHERE index_text <> content)`).Scan(&nulls, &validFrom, &chunks, &noIndex); err != nil {
				t.Fatal(err)
			}
			if nulls != 0 || validFrom != 0 || chunks != 7 || noIndex != 0 {
				t.Fatalf("documents without a base %d, with a valid_from %d, chunks %d, index_text different from the body %d", nulls, validFrom, chunks, noIndex)
			}
			rows, _ := conn.Query(ctx, `SELECT t.slug, kb.name, kb.scope_kind, count(d.id) FROM knowledge_bases kb JOIN tenants t ON t.id = kb.tenant_id LEFT JOIN knowledge_documents d ON d.knowledge_base_id = kb.id GROUP BY 1,2,3 ORDER BY 1,2`)
			var got []string
			for rows.Next() {
				var slug, name, kind string
				var n int
				_ = rows.Scan(&slug, &name, &kind, &n)
				got = append(got, fmt.Sprintf("%s/%s/%s/%d", slug, name, kind, n))
			}
			rows.Close()
			if want := "acme/handbook/tenant/5 acme/member-notes/tenant/1 globex/handbook/tenant/1"; strings.Join(got, " ") != want {
				t.Fatalf("bases %v, want %s (empty-src has no documents and so no base)", got, want)
			}
			// the visibility of what was indexed did not change
			for who, want := range map[string][]string{"carol": {"openmigr", "newversion", "notesmigr"}, "dan": {"openmigr", "aclmigr", "newversion", "notesmigr"}} {
				got := fmt.Sprint(items(resolveKnowledge(e, who, "openmigr aclmigr vermigr revmigr notesmigr globexmigr", nil)))
				for _, w := range want {
					if !strings.Contains(got, w) {
						t.Errorf("%s lost %s", who, w)
					}
				}
				for _, w := range []string{"oldversion", "revmigr", "globexmigr"} {
					if strings.Contains(got, w) {
						t.Errorf("%s gained %s", who, w)
					}
				}
				if who == "carol" && strings.Contains(got, "aclmigr") {
					t.Error("carol gained aclmigr")
				}
			}
			// a document registered ten days ago was valid before that, as it always was
			old := time.Now().AddDate(0, 0, -30).UTC().Format(time.RFC3339)
			if !has(resolveKnowledge(e, "carol", "openmigr", map[string]any{"asOf": map[string]any{"valid": old}}), "openmigr") {
				t.Error("a migrated document without validFrom is not valid before its registration")
			}
			// the replaced version of the old release is shown as superseded and the new one as active
			status := map[string]any{}
			for _, d := range e.must(200, "GET", "/api/v1/knowledge/documents?limit=100", "alice", nil)["documents"].([]any) {
				m := d.(map[string]any)
				if m["uri"] == "handbook://ver" {
					status[fmt.Sprint(m["version"])] = m["status"]
				}
			}
			if status["1"] != "superseded" || status["2"] != "active" {
				t.Errorf("statuses %v", status)
			}
			// new uploads into a migrated base and a first personal upload work
			e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://after", "", "aftermigr"))
			e.must(201, "POST", "/v1/knowledge/documents", "bob", kbDoc("member-notes", "kb://me/after", "", "bobaftermigr"))
			if !kbSees(e, "carol", "aftermigr") {
				t.Error("a document added after the migration is not retrieved")
			}
			// a new version of a migrated document continues its numbering
			v3 := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://ver", "", "vermigr thirdversion"))
			if v3["version"] != float64(3) {
				t.Errorf("version after the migration: %v", v3["version"])
			}
			// the row-level security is back on the tables the migration switched it off for
			var forced int
			_ = conn.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname IN ('sources', 'knowledge_documents', 'knowledge_chunks', 'knowledge_bases') AND relrowsecurity AND relforcerowsecurity`).Scan(&forced)
			if forced != 4 {
				t.Errorf("FORCE row-level security is on for %d of 4 tables", forced)
			}
		})
	}
}
