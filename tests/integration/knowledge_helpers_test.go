package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/adapters/identity"
	"idp-cmi/internal/authorization"
	"idp-cmi/internal/cmi"
	"idp-cmi/internal/convert/convtest"
)

// addUser gives a subject of tenant A a token with the roles (reader when none).
func (e *env) addUser(name string, roles ...string) error {
	if len(roles) == 0 {
		roles = []string{authorization.RoleReader}
	}
	tok, err := identity.DevToken([]byte(devSecret), profile, name, e.tenantA, roles, time.Hour)
	if err != nil {
		return err
	}
	e.tokens[name] = tok
	return nil
}

// kbSources registers the two sources the knowledge tests use: an enterprise document store and a user source.
func kbSources(e *env) {
	e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "handbook", "displayName": "Handbook", "kind": "document-store", "trust": "authoritative"})
	e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "member-notes", "displayName": "Member notes", "kind": "user", "trust": "informational"})
}

// kbBase is a base as the administrator registers it.
func kbBase(name, kind, id, source string, managers map[string]any, quotaDocs int, quotaBytes int64) map[string]any {
	scope := map[string]any{"kind": kind}
	if id != "" {
		scope["id"] = id
	}
	if managers == nil {
		managers = map[string]any{}
	}
	return map[string]any{
		"name": name, "displayName": name, "scope": scope, "managers": managers,
		"defaultSensitivity": "internal", "sensitivityCeiling": "restricted", "source": source,
		"quotaDocuments": quotaDocs, "quotaBytes": quotaBytes, "ocr": false,
	}
}

// kbDoc is a document upload.
func kbDoc(source, uri, base, text string) map[string]any {
	body := map[string]any{"source": source, "uri": uri, "title": uri, "sensitivity": "internal", "acl": map[string]any{}, "text": text}
	if base != "" {
		body["knowledgeBase"] = base
	}
	return body
}

// kbSees reports whether word is in what who's resolve of word returns from knowledge; the extra scopes are named in the request.
func kbSees(e *env, who, word string, scopes ...map[string]any) bool {
	e.t.Helper()
	body := map[string]any{"query": word, "kinds": []string{"knowledge"}}
	if len(scopes) > 0 {
		body["scopes"] = scopes
	}
	return strings.Contains(fmt.Sprint(items(e.must(200, "POST", "/v1/context/resolve", who, body))), word)
}

// ownBase is the name of the personal base who sees in their list of bases.
func ownBase(e *env, who string) string {
	e.t.Helper()
	for _, b := range e.must(200, "GET", "/api/v1/knowledge/bases", who, nil)["knowledgeBases"].([]any) {
		m := b.(map[string]any)
		if m["scope"].(map[string]any)["kind"] == "subject" {
			return m["name"].(string)
		}
	}
	e.t.Fatalf("%s has no personal base", who)
	return ""
}

// auditCount is how many audit events have the action (and, when resource is not empty, that resource).
func auditCount(e *env, action, resource string) int {
	e.t.Helper()
	n := 0
	for _, ev := range e.must(200, "GET", "/api/v1/audit?limit=1000", "alice", nil)["events"].([]any) {
		m := ev.(map[string]any)
		if m["action"] == action && (resource == "" || m["resourceId"] == resource) {
			n++
		}
	}
	return n
}

func errCode(out map[string]any) string {
	if m, ok := out["error"].(map[string]any); ok {
		s, _ := m["code"].(string)
		return s
	}
	return ""
}

func mustFail(t *testing.T, st int, out map[string]any, want int, code string) {
	t.Helper()
	if st != want || errCode(out) != code {
		t.Fatalf("want %d %s, got %d %v", want, code, st, out)
	}
}

func writeFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := dir + "/" + name
	if err := os.WriteFile(p, []byte("secret-"+name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// ownerQuery runs a query that returns one row as the schema owner (which sees every tenant) and scans it into dest.
func ownerQuery(t *testing.T, ownerURL, sql string, dest any, args ...any) error {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	return conn.QueryRow(ctx, sql, args...).Scan(dest)
}

// decodeBody reads a JSON object.
func decodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	return out
}

type pgxTx = pgx.Tx

func convtestOutput(md string) convtest.Output { return convtest.Output{Markdown: md} }

func cmiInfo() cmi.ExtractInfo { return cmi.ExtractInfo{HasBody: true} }

// ownerExec runs a statement as the schema owner.
func ownerExec(t *testing.T, ownerURL, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}
