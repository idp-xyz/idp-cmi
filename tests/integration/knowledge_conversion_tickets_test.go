package integration

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/ingestfiles"
	"idp-cmi/internal/ticket"
)

// running is a document with an original and a job of it that a worker holds (attempt 1), so tickets can be made for it.
type running struct {
	id   string
	job  postgres.Conversion
	body string
}

func (c *conv) running(who, path, body string) running {
	c.t.Helper()
	id := c.stub(who, path, "placeholder")
	c.put(who, id, mediaPDF, body)
	jobs, err := c.e.store.ClaimConversions(context.Background(), "it-claim", 50)
	if err != nil {
		c.t.Fatal(err)
	}
	for _, j := range jobs {
		if j.DocumentID == id {
			return running{id: id, job: j, body: body}
		}
	}
	c.t.Fatalf("no job of %s claimed", id)
	return running{}
}

func (c *conv) claims(r running, kind string) ticket.Claims {
	j := r.job
	return ticket.Claims{Tenant: "system", Subject: "svc-cmi-ingest", Kind: kind, DocTenant: j.Tenant, Document: j.DocumentID, Version: j.Version, Job: j.ID,
		Attempt: j.Attempts, Key: j.OriginalKey, SHA256: j.OriginalSHA256}
}

func (c *conv) tok(cl ticket.Claims) string {
	c.t.Helper()
	tok, _, err := c.signer.Sign(cl)
	if err != nil {
		c.t.Fatal(err)
	}
	return tok
}

func (c *conv) get(tok string) (int, []byte, http.Header) {
	c.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, c.files.URL+"/content", nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func (c *conv) post(tok string, body []byte) (int, []byte) {
	c.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.files.URL+"/", bytes.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestAReadTicketServesItsOriginalOnceWithTheHeadersTheSandboxReads(t *testing.T) {
	c := newConv(t)
	r := c.running("bob", "a", "%PDF-1.4 ZqxSecretOriginal")
	tok := c.tok(c.claims(r, ticket.Read))
	st, body, h := c.get(tok)
	if st != 200 || string(body) != r.body || h.Get("Content-Type") != mediaPDF || h.Get("Idp-File-Ref") != r.id ||
		!strings.Contains(h.Get("Content-Disposition"), `filename=original.pdf`) || len(h.Get("Idp-File-Sha256")) != 64 {
		t.Fatalf("%d %q %v", st, body, h)
	}
	if st, body, _ := c.get(tok); st != 403 || strings.Contains(string(body), "ZqxSecretOriginal") {
		t.Fatalf("a ticket works once: %d %s", st, body)
	}
	var uses int
	if err := ownerQuery(t, c.e.ownerURL, `SELECT count(*) FROM knowledge_ticket_uses WHERE document_id = $1`, &uses, r.id); err != nil || uses != 1 {
		t.Fatalf("the use is recorded: %d %v", uses, err)
	}
}

func TestATicketPresentedToAnotherReplicaIsStillUsed(t *testing.T) {
	c := newConv(t)
	r := c.running("bob", "a", "%PDF ZqxX")
	tok := c.tok(c.claims(r, ticket.Read))
	if st, _, _ := c.get(tok); st != 200 {
		t.Fatalf("%d", st)
	}
	// another replica (or this one after a restart) has nothing in memory, only the database
	other := &ingestfiles.Handler{Backend: c.e.store, Verifier: &ticket.Verifier{Key: ticketKey, Audience: c.files.URL}}
	req, _ := http.NewRequest(http.MethodGet, "/content", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := &recorder{header: http.Header{}}
	other.ServeHTTP(rec, req)
	if rec.status != 403 {
		t.Fatalf("a used ticket on another replica: %d", rec.status)
	}
}

type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(s int)           { r.status = s }

func TestForgedExpiredWrongKindAndWrongAudienceTicketsAreRefused(t *testing.T) {
	c := newConv(t)
	r := c.running("bob", "a", "%PDF ZqxNeverServed")
	good := c.claims(r, ticket.Read)
	tok := c.tok(good)
	other := &ticket.Signer{Key: []byte("ffffffffffffffffffffffffffffffff"), Audience: c.files.URL}
	forgedKey, _, _ := other.Sign(good)
	badSig := tok[:len(tok)-3] + "AAA"
	expired := &ticket.Signer{Key: ticketKey, Audience: c.files.URL, TTL: time.Minute, Now: func() time.Time { return time.Now().Add(-time.Hour) }}
	expiredTok, _, _ := expired.Sign(good)
	elsewhere := &ticket.Signer{Key: ticketKey, Audience: "http://some-other-endpoint", TTL: time.Minute}
	elsewhereTok, _, _ := elsewhere.Sign(good)
	writeTok := c.tok(c.claims(r, ticket.Write))
	for name, bad := range map[string]string{"a forged signature": badSig, "another key": forgedKey, "an expired ticket": expiredTok, "another audience": elsewhereTok,
		"a write ticket to read": writeTok, "garbage": "a.b.c", "alg none": "eyJhbGciOiJub25lIn0.e30."} {
		if st, body, _ := c.get(bad); st != 403 || strings.Contains(string(body), "ZqxNeverServed") {
			t.Errorf("%s: %d %s", name, st, body)
		}
	}
	if st, _, _ := c.get(""); st != 401 {
		t.Errorf("no ticket: %d", st)
	}
	if st, _ := c.post(tok, []byte("# text")); st != 403 {
		t.Errorf("a read ticket to write: %d", st)
	}
	// none of those spent the good ticket
	if st, _, _ := c.get(tok); st != 200 {
		t.Fatalf("the good ticket still works: %d", st)
	}
}

func TestATicketThatDoesNotFitItsJobIsRefused(t *testing.T) {
	c := newConv(t)
	r := c.running("bob", "a", "%PDF ZqxBound")
	otherTenantDoc := func() string {
		d := c.e.must(201, "POST", "/api/v1/knowledge/documents", "erin", kbDoc("handbook", "h://x", "", "text of tenant B"))
		return d["documentId"].(string)
	}
	c.e.must(201, "POST", "/api/v1/sources", "erin", map[string]any{"name": "handbook", "displayName": "Handbook", "kind": "document-store", "trust": "authoritative"})
	foreign := otherTenantDoc()
	mod := map[string]func(*ticket.Claims){
		"another document":      func(cl *ticket.Claims) { cl.Document = foreign },
		"another tenant":        func(cl *ticket.Claims) { cl.DocTenant = c.e.tenantB },
		"another version":       func(cl *ticket.Claims) { cl.Version = 9 },
		"another key":           func(cl *ticket.Claims) { cl.Key = "kb/" + r.job.Tenant + "/" + foreign + "/1/x" },
		"another digest":        func(cl *ticket.Claims) { cl.SHA256 = "sha256:" + strings.Repeat("0", 64) },
		"another attempt":       func(cl *ticket.Claims) { cl.Attempt = 2 },
		"a job that is not one": func(cl *ticket.Claims) { cl.Job = "00000000-0000-4000-8000-000000000000" },
		"a job that is no id":   func(cl *ticket.Claims) { cl.Job = "../../etc" },
	}
	for name, f := range mod {
		cl := c.claims(r, ticket.Read)
		f(&cl)
		if st, body, _ := c.get(c.tok(cl)); st != 403 || strings.Contains(string(body), "ZqxBound") {
			t.Errorf("%s: %d %s", name, st, body)
		}
	}
	if st, _, _ := c.get(c.tok(c.claims(r, ticket.Read))); st != 200 {
		t.Fatalf("the right ticket works: %d", st)
	}
}

func TestATicketOfAJobThatIsNotRunningOrWhoseOriginalWasReplacedOrErasedIsRefused(t *testing.T) {
	c := newConv(t)
	// queued, not running
	id := c.stub("bob", "q", "placeholder")
	c.put("bob", id, mediaPDF, "%PDF ZqxQueued")
	var jobID string
	if err := ownerQuery(t, c.e.ownerURL, `SELECT id::text FROM knowledge_conversions WHERE document_id = $1`, &jobID, id); err != nil {
		t.Fatal(err)
	}
	var key, sum, tenant string
	var ver int
	if err := ownerQuery(t, c.e.ownerURL, `SELECT original_key FROM knowledge_conversions WHERE id = $1`, &key, jobID); err != nil {
		t.Fatal(err)
	}
	_ = ownerQuery(t, c.e.ownerURL, `SELECT original_sha256 FROM knowledge_conversions WHERE id = $1`, &sum, jobID)
	_ = ownerQuery(t, c.e.ownerURL, `SELECT tenant_id::text FROM knowledge_conversions WHERE id = $1`, &tenant, jobID)
	_ = ownerQuery(t, c.e.ownerURL, `SELECT document_version FROM knowledge_conversions WHERE id = $1`, &ver, jobID)
	queued := ticket.Claims{Tenant: "system", Subject: "svc-cmi-ingest", Kind: ticket.Read, DocTenant: tenant, Document: id, Version: ver, Job: jobID, Attempt: 0, Key: key, SHA256: sum}
	if st, _, _ := c.get(c.tok(queued)); st != 403 {
		t.Errorf("queued: %d", st)
	}
	queued.Attempt = 1
	if st, _, _ := c.get(c.tok(queued)); st != 403 {
		t.Errorf("queued, with the attempt it would have: %d", st)
	}
	// replaced original: a ticket for the first attempt
	r := c.running("bob", "r", "%PDF ZqxFirst")
	c.put("bob", r.id, mediaPDF, "%PDF ZqxSecond")
	if st, body, _ := c.get(c.tok(c.claims(r, ticket.Read))); st != 403 || strings.Contains(string(body), "Zqx") {
		t.Errorf("replaced: %d %s", st, body)
	}
	// erased document
	e := c.running("bob", "e", "%PDF ZqxErased")
	c.e.must(201, "POST", "/api/v1/erasures", "alice", map[string]any{"documentIds": []string{e.id}, "reason": "test"})
	if st, body, _ := c.get(c.tok(c.claims(e, ticket.Read))); st != 403 || strings.Contains(string(body), "ZqxErased") {
		t.Errorf("erased: %d %s", st, body)
	}
	// a finished job
	f := c.running("bob", "f", "%PDF ZqxFinished")
	c.output("ZqxFinished", convtestOutput("<!-- page: 1 -->\nsome text\n"))
	if _, err := c.e.store.CommitConversion(context.Background(), f.job, "<!-- page: 1 -->\nsome text", cmiInfo()); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := c.get(c.tok(c.claims(f, ticket.Read))); st != 403 {
		t.Errorf("finished: %d", st)
	}
}

func TestAWriteTicketStagesOneResultWithinTheLimit(t *testing.T) {
	c := newConv(t)
	c.handler.MaxResult = 1000
	r := c.running("bob", "w", "%PDF ZqxW")
	if st, _ := c.post(c.tok(c.claims(r, ticket.Write)), nil); st != 422 {
		t.Errorf("an empty result: %d", st)
	}
	if st, _ := c.post(c.tok(c.claims(r, ticket.Write)), bytes.Repeat([]byte("x"), 1001)); st != 413 {
		t.Errorf("a result over the limit: %d", st)
	}
	// a body that does not say how long it is (chunked) is bounded as it is read
	req, _ := http.NewRequest(http.MethodPost, c.files.URL+"/", io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), 1001))))
	req.Header.Set("Authorization", "Bearer "+c.tok(c.claims(r, ticket.Write)))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 413 {
		t.Errorf("a chunked result over the limit: %v %v", resp, err)
	}
	if st, body := c.post(c.tok(c.claims(r, ticket.Write)), []byte("# text\nZqxResult")); st != 200 || !strings.Contains(string(body), `"ref":"`+r.job.ID+`"`) || !strings.Contains(string(body), `"size":16`) {
		t.Fatalf("%d %s", st, body)
	}
	if st, _ := c.post(c.tok(c.claims(r, ticket.Write)), []byte("# another")); st != 403 {
		t.Errorf("a second result for the same attempt: %d", st)
	}
	if keys := c.mem.Keys(); len(keys) != 2 {
		t.Fatalf("the original and one staged result: %v", keys)
	}
	for _, k := range c.mem.Keys() {
		if !strings.HasPrefix(k, "kb/"+r.job.Tenant+"/"+r.id+"/") {
			t.Fatalf("staged under the document's prefix, where an erasure sweeps: %s", k)
		}
	}
}

func TestTheSandboxRefusesTicketsOfAnotherCaller(t *testing.T) {
	c := newConv(t)
	c.worker.WorkloadSubject = "someone-else" // the tickets then name a caller the gateway does not assert
	id := c.stub("bob", "d", "placeholder")
	c.put("bob", id, mediaPDF, "%PDF ZqxD")
	c.drain()
	in := ingestOf(c.doc("bob", id))
	if in["state"] != "failed" || in["error"] != "import_refused" {
		t.Fatalf("%v", in)
	}
}

func TestConversionTablesAreTenantIsolated(t *testing.T) {
	c := newConv(t)
	r := c.running("bob", "a", "%PDF ZqxA")
	if st, _, _ := c.get(c.tok(c.claims(r, ticket.Read))); st != 200 { // a ticket is presented, so that its record exists
		t.Fatalf("%d", st)
	}
	var visible int
	conn, err := c.e.store.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, q := range []string{`SELECT count(*) FROM knowledge_conversions`, `SELECT count(*) FROM knowledge_ticket_uses`} {
		if err := conn.QueryRow(context.Background(), q).Scan(&visible); err != nil || visible != 0 {
			t.Fatalf("%s without a tenant sees %d rows (%v)", q, visible, err)
		}
	}
	for _, table := range []string{"knowledge_conversions", "knowledge_ticket_uses"} {
		if err := c.e.store.InTenant(context.Background(), c.e.tenantB, func(tx pgxTx) error {
			return tx.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&visible)
		}); err != nil || visible != 0 {
			t.Fatalf("another tenant sees %d rows of %s (%v)", visible, table, err)
		}
	}
	if err := c.e.store.InTenant(context.Background(), c.e.tenantA, func(tx pgxTx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM knowledge_ticket_uses`).Scan(&visible)
	}); err != nil || visible != 1 {
		t.Fatalf("the tenant sees its own record: %d (%v)", visible, err)
	}
}
