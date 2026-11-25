package integration

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"idp-cmi/adapters/blob"
)

// S3: erasing a subject erases their personal knowledge base - documents, chunks, originals - and the owner fields
// that name them; the erasure is verified only when all of it is gone, including what arrived after the request.
func TestSubjectErasureCoversThePersonalBase(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	mem := &blob.Mem{}
	e.store.Originals = mem
	a := e.must(201, "POST", "/v1/knowledge/documents", "bob", kbDoc("member-notes", "kb://me/a", "", "erasequartz first note"))
	b := e.must(201, "POST", "/v1/knowledge/documents", "bob", kbDoc("member-notes", "kb://me/b", "", "erasequartz second note"))
	for _, d := range []map[string]any{a, b} {
		if st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+d["documentId"].(string)+"/original", "bob", "application/pdf", []byte("%PDF erasequartz")); st != 200 {
			t.Fatal(st)
		}
	}
	carol := e.must(201, "POST", "/v1/knowledge/documents", "carol", kbDoc("member-notes", "kb://me/c", "", "erasequartz carol's own note"))
	owned := kbDoc("handbook", "handbook://owned", "", "ownedquartz a document bob is responsible for")
	owned["owner"] = "bob"
	ownedDoc := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", owned)
	if ownedDoc["owner"] != "bob" || len(mem.Keys()) != 2 {
		t.Fatalf("setup: %v %v", ownedDoc["owner"], mem.Keys())
	}

	er := e.must(201, "POST", "/api/v1/erasures", "alice", map[string]any{"subject": "bob", "reason": "left the company"})
	// a document that reaches the base after the request is under the erasure too
	late := e.must(201, "POST", "/v1/knowledge/documents", "bob", kbDoc("member-notes", "kb://me/late", "", "erasequartz arrived late"))
	if st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+late["documentId"].(string)+"/original", "bob", "application/pdf", []byte("%PDF late")); st != 200 {
		t.Fatal(st)
	}
	if _, err := e.store.Purge(context.Background(), e.tenantA, 5); err != nil {
		t.Fatal(err)
	}
	got := e.must(200, "GET", "/api/v1/erasures/"+er["erasureId"].(string), "alice", nil)
	if got["status"] != "verified" {
		t.Fatalf("erasure %v", got)
	}
	if len(mem.Keys()) != 0 {
		t.Fatalf("originals remain: %v", mem.Keys())
	}
	conn, err := pgx.Connect(context.Background(), e.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var open, text, originals int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE d.erasure_id IS NULL), count(c.document_id) FILTER (WHERE c.content <> '[erased]' OR c.index_text <> '[erased]'), count(*) FILTER (WHERE d.original_key IS NOT NULL)
		FROM knowledge_documents d LEFT JOIN knowledge_chunks c ON c.document_id = d.id WHERE d.id = ANY($1::uuid[])`, []string{a["documentId"].(string), b["documentId"].(string), late["documentId"].(string)}).Scan(&open, &text, &originals); err != nil {
		t.Fatal(err)
	}
	if open != 0 || text != 0 || originals != 0 {
		t.Fatalf("bob's base still holds: %d documents outside the erasure, %d chunks with text, %d originals", open, text, originals)
	}
	var owner string
	if err := conn.QueryRow(context.Background(), `SELECT owner FROM knowledge_documents WHERE id = $1`, ownedDoc["documentId"]).Scan(&owner); err != nil || owner != "" {
		t.Fatalf("owner field %q %v", owner, err)
	}
	// what is not bob's stays
	if !kbSees(e, "carol", "ownedquartz") || e.must(200, "GET", "/v1/knowledge/documents/"+carol["documentId"].(string), "carol", nil)["documentId"] == nil {
		t.Fatal("the erasure took more than the subject's own")
	}
	if kbSees(e, "alice", "erasequartz", map[string]any{"kind": "subject", "id": "bob"}) {
		t.Fatal("erased content is retrieved")
	}
}
