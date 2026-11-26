package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"idp-cmi/adapters/blob"
	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/cmi"
	"idp-cmi/internal/convert"
	"idp-cmi/internal/convert/convtest"
	"idp-cmi/internal/ticket"
)

func TestVersionsWaitingForTheirTextCountAgainstTheQuota(t *testing.T) {
	c := newConv(t)
	c.e.must(201, "POST", "/api/v1/knowledge/bases", "alice", kbBase("small", "tenant", "", "handbook", map[string]any{"roles": []string{"cmi-admin"}}, 0, 3000))
	body := "%PDF " + strings.Repeat("x", 2500)
	accepted := 0
	for i := 0; i < 4; i++ {
		doc := kbDoc("handbook", fmt.Sprintf("kb://small/doc-%d", i), "small", "placeholder")
		doc["awaitsOriginal"] = true
		d := c.e.must(201, "POST", "/api/v1/knowledge/documents", "alice", doc)
		st, _, _ := c.e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+d["documentId"].(string)+"/original", "alice", mediaPDF, []byte(body))
		if st == 200 {
			accepted++
		} else if st != 429 {
			t.Fatalf("%d", st)
		}
	}
	if accepted != 1 {
		t.Fatalf("%d originals of 2.5 KB fitted a quota of 3000 bytes: versions that wait for their text are counted", accepted)
	}
}

func TestAConversionOfAVersionThatStartsLaterReplacesNothingYetAndFreesNothing(t *testing.T) {
	c := newConv(t)
	c.e.must(201, "POST", "/api/v1/knowledge/bases", "alice", kbBase("ops", "tenant", "", "handbook", map[string]any{"roles": []string{"cmi-admin"}}, 0, 0))
	c.e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "kb://ops/rates", "ops", "the older ZqxOldRates rates"))
	later := kbDoc("handbook", "kb://ops/rates", "ops", "placeholder")
	later["validFrom"] = time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	later["awaitsOriginal"] = true
	d := c.e.must(201, "POST", "/api/v1/knowledge/documents", "alice", later)
	id := d["documentId"].(string)
	c.output("TAG", convtest.Output{Markdown: "<!-- page: 1 -->\nThe ZqxFutureRates rates.\n"})
	if st, _, raw := c.e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "alice", mediaPDF, []byte("%PDF TAG")); st != 200 {
		t.Fatalf("%d %s", st, raw)
	}
	c.drain()
	doc := c.e.must(200, "GET", "/api/v1/knowledge/documents/"+id, "alice", nil)
	if doc["status"] != "active" || ingestOf(doc)["state"] != "succeeded" {
		t.Fatalf("%v", doc)
	}
	if len(c.hits("alice", "ZqxOldRates")) != 1 || len(c.hits("alice", "ZqxFutureRates")) != 0 {
		t.Fatal("a version that starts later does not replace the one in force (D52), converted or not")
	}
}

func TestAPendingVersionIsNotGivenAnOriginalThatIsNeverConverted(t *testing.T) {
	c := newConv(t)
	c.output("TAG", convtest.Output{Exit: 4, Stderr: "pdftext: error: corrupt"})
	id := c.stub("bob", "doc", "placeholder")
	c.put("bob", id, mediaPDF, "%PDF TAG")
	c.drain() // failed, still pending
	for _, media := range []string{"text/markdown", "image/png", "text/plain"} {
		st, _, raw := c.e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", media, []byte("# text"))
		if st != 409 || !strings.Contains(string(raw), "waiting for the text") {
			t.Fatalf("%s: %d %s", media, st, raw)
		}
	}
	if d := c.doc("bob", id); d["status"] != "pending" || len(c.mem.Keys()) != 1 {
		t.Fatalf("the refused upload left nothing: %v %v", d, c.mem.Keys())
	}
	// another PDF is fine, and so is any original once the text has come
	c.output("TAG", convtest.Output{Markdown: "<!-- page: 1 -->\nZqxFine text.\n"})
	c.put("bob", id, mediaPDF, "%PDF TAG")
	c.drain()
	c.put("bob", id, "text/markdown", "# a text original on an extracted version")
}

func TestStagedTextIsDeletedWhenItsJobEnds(t *testing.T) {
	c := newConv(t)
	// replaced after staging
	r := c.running("bob", "a", "%PDF ZqxA")
	if st, _ := c.post(c.tok(c.claims(r, ticket.Write)), []byte("<!-- page: 1 -->\nZqxStagedA\n")); st != 200 {
		t.Fatal(st)
	}
	c.put("bob", r.id, mediaPDF, "%PDF ZqxA2")
	for _, k := range c.mem.Keys() {
		if strings.Contains(k, "result-") {
			t.Fatalf("a given-up job's staged text is left: %v", c.mem.Keys())
		}
	}
	// failed
	f := c.running("bob", "b", "%PDF ZqxB")
	if st, _ := c.post(c.tok(c.claims(f, ticket.Write)), []byte("<!-- page: 1 -->\nZqxStagedB\n")); st != 200 {
		t.Fatal(st)
	}
	if err := c.e.store.FailConversion(context.Background(), f.job, postgres.ConversionError{Code: "x", Detail: "d"}); err != nil {
		t.Fatal(err)
	}
	for _, k := range c.mem.Keys() {
		if strings.Contains(k, "result-") {
			t.Fatalf("a failed job's staged text is left: %v", c.mem.Keys())
		}
	}
}

func TestAJobThatDiedOnItsLastAttemptFailsAtClaimAndLosesItsStagedText(t *testing.T) {
	c := newConv(t)
	c.e.store.Conversion.MaxAttempts = 1
	c.e.store.Conversion.Lease = time.Second
	r := c.running("bob", "a", "%PDF ZqxA")
	if st, _ := c.post(c.tok(c.claims(r, ticket.Write)), []byte("<!-- page: 1 -->\nZqxStaged\n")); st != 200 {
		t.Fatal(st)
	}
	time.Sleep(1200 * time.Millisecond)
	if jobs, err := c.e.store.ClaimConversions(context.Background(), "next", 5); err != nil || len(jobs) != 0 {
		t.Fatalf("%v %v", jobs, err)
	}
	if in := ingestOf(c.doc("bob", r.id)); in["state"] != "failed" || in["error"] != "attempts_exhausted" {
		t.Fatalf("%v", in)
	}
	for _, k := range c.mem.Keys() {
		if strings.Contains(k, "result-") {
			t.Fatalf("staged text left: %v", c.mem.Keys())
		}
	}
}

func TestABusySandboxDoesNotSpendAttemptsButDoesNotWaitForEver(t *testing.T) {
	c := newConv(t)
	c.e.store.Conversion.MaxAttempts = 2
	var busy atomic.Int32
	c.sb.Before = func(tool string, _ map[string]any) *convtest.Reply {
		if tool == "sandbox_create" && busy.Add(1) <= 5 {
			return &convtest.Reply{IsError: true, Text: "the sandbox is at capacity; try again later"}
		}
		return nil
	}
	c.output("TAG", convtest.Output{Markdown: "<!-- page: 1 -->\nZqxPatient text.\n"})
	id := c.stub("bob", "doc", "placeholder")
	c.put("bob", id, mediaPDF, "%PDF TAG")
	c.drain()
	if in := ingestOf(c.doc("bob", id)); in["state"] != "succeeded" || in["attempts"].(float64) != 1 {
		t.Fatalf("five busy answers with two attempts allowed: %v", in)
	}
	// a job that has waited for longer than the cap fails with the busy code
	c2 := newConv(t)
	c2.e.store.Conversion.MaxBusyWait = time.Millisecond
	c2.sb.Before = func(tool string, _ map[string]any) *convtest.Reply {
		return &convtest.Reply{IsError: true, Text: "the sandbox is at capacity; try again later"}
	}
	id = c2.stub("bob", "doc", "placeholder")
	c2.put("bob", id, mediaPDF, "%PDF TAG")
	c2.drain()
	if in := ingestOf(c2.doc("bob", id)); in["state"] != "failed" || in["error"] != "sandbox_busy" {
		t.Fatalf("%v", in)
	}
}

func TestSandboxConcurrencyHoldsOverReplicas(t *testing.T) {
	c := newConv(t)
	c.e.store.Conversion.SandboxConcurrency = 2
	for i := 0; i < 5; i++ {
		id := c.stub("bob", fmt.Sprintf("d%d", i), "placeholder")
		c.put("bob", id, mediaPDF, "%PDF TAG")
	}
	a, err := c.e.store.ClaimConversions(context.Background(), "replica-a", 5)
	if err != nil || len(a) != 2 {
		t.Fatalf("%d %v", len(a), err)
	}
	if b, err := c.e.store.ClaimConversions(context.Background(), "replica-b", 5); err != nil || len(b) != 0 {
		t.Fatalf("another replica took %d more while two are running: %v", len(b), err)
	}
}

func TestAnInterruptedAttemptIsGivenBackUnspent(t *testing.T) {
	c := newConv(t)
	ctx, cancel := context.WithCancel(context.Background())
	c.sb.Before = func(tool string, _ map[string]any) *convtest.Reply {
		if tool == "sandbox_run" {
			cancel() // CMI is shutting down while the converter runs
			return &convtest.Reply{Hang: 300 * time.Millisecond}
		}
		return nil
	}
	id := c.stub("bob", "doc", "placeholder")
	c.put("bob", id, mediaPDF, "%PDF TAG")
	var wg sync.WaitGroup
	c.worker.Tick(ctx, &wg)
	wg.Wait()
	in := ingestOf(c.doc("bob", id))
	if in["state"] != "queued" || in["attempts"].(float64) != 0 || len(c.Outcomes()) != 0 {
		t.Fatalf("%v %v", in, c.Outcomes())
	}
	if c.sb.Workspaces() != 0 {
		t.Fatal("the workspace is deleted")
	}
}

func TestATicketShapedTextIsNeverStored(t *testing.T) {
	c := newConv(t)
	r := c.running("bob", "a", "%PDF ZqxA")
	tok := c.tok(c.claims(r, ticket.Read))
	if err := c.e.store.FailConversion(context.Background(), r.job, postgres.ConversionError{Code: "import_refused", Detail: "refused " + tok + " for good"}); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := ownerQuery(t, c.e.ownerURL, `SELECT error_detail || ' ' || (SELECT string_agg(detail::text, ' ') FROM audit_events WHERE resource_id = $1 AND action = 'knowledge.ingest.failed') FROM knowledge_conversions WHERE id = $2`, &stored, r.id, r.job.ID); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "eyJ") || !strings.Contains(stored, "[redacted]") {
		t.Fatalf("%s", stored)
	}
}

func TestControlCharactersAndHugeMarkerNumbersDoNotFailTheCommit(t *testing.T) {
	c := newConv(t)
	c.output("TAG", convtest.Output{Markdown: "<!-- page: 99999999999999999999 -->\nZqxNul\x00Word and \x01 more\x1b text.\n<!-- slide: 4294967296 -->\nZqxSlideWord\n<!-- page: 2 -->\nZqxTwo\n"})
	id := c.stub("bob", "doc", "placeholder")
	c.put("bob", id, mediaPDF, "%PDF TAG")
	c.drain()
	in := ingestOf(c.doc("bob", id))
	if in["state"] != "succeeded" || in["attempts"].(float64) != 1 {
		t.Fatalf("%v", in)
	}
	if hs := c.hits("bob", "ZqxTwo"); len(hs) != 1 || locatorOf(hs[0])["page"].(float64) != 2 {
		t.Fatalf("%v", hs)
	}
}

// the same ticket presented twice at once: one wins
func TestAWriteTicketPresentedConcurrentlyIsUsedOnce(t *testing.T) {
	c := newConv(t)
	r := c.running("bob", "w", "%PDF ZqxW")
	tok := c.tok(c.claims(r, ticket.Write))
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if st, _ := c.post(tok, []byte("<!-- page: 1 -->\nZqxConc\n")); st == 200 {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("%d presentations were accepted", ok.Load())
	}
	staged := 0
	for _, k := range c.mem.Keys() {
		if strings.Contains(k, "result-") {
			staged++
		}
	}
	if staged != 1 {
		t.Fatalf("%d staged results", staged)
	}
}

func TestATicketPresentedAfterItsLeaseLapsedIsRefused(t *testing.T) {
	c := newConv(t)
	c.e.store.Conversion.Lease = time.Second
	r := c.running("bob", "a", "%PDF ZqxLate")
	tok := c.tok(c.claims(r, ticket.Read))
	time.Sleep(1200 * time.Millisecond)
	if st, body, _ := c.get(tok); st != 403 || strings.Contains(string(body), "ZqxLate") {
		t.Fatalf("%d %s", st, body)
	}
}

// a blocking backend, to hold reads open
type slowReads struct {
	*postgres.Store
	gate chan struct{}
	in   chan struct{}
	err  error
}

func (s *slowReads) ConversionOriginal(ctx context.Context, cl ticket.Claims) (*os.File, string, int64, error) {
	if s.err != nil {
		return nil, "", 0, s.err
	}
	if s.in != nil {
		s.in <- struct{}{}
		<-s.gate
	}
	return s.Store.ConversionOriginal(ctx, cl)
}

func TestTheEndpointAtItsLimitOfReadsTurnsAwayWithoutUsingTheTicket(t *testing.T) {
	c := newConv(t)
	slow := &slowReads{Store: c.e.store, gate: make(chan struct{}), in: make(chan struct{}, 1)}
	c.handler.Backend = slow
	c.handler.MaxReads = 1
	a := c.running("bob", "a", "%PDF ZqxA")
	b := c.running("bob", "b", "%PDF ZqxB")
	done := make(chan int)
	go func() { st, _, _ := c.get(c.tok(c.claims(a, ticket.Read))); done <- st }()
	<-slow.in // the first read holds the place
	tokB := c.tok(c.claims(b, ticket.Read))
	if st, _, _ := c.get(tokB); st != 429 {
		t.Fatalf("%d", st)
	}
	slow.in = nil
	close(slow.gate)
	if st := <-done; st != 200 {
		t.Fatalf("%d", st)
	}
	if st, _, _ := c.get(tokB); st != 200 {
		t.Fatalf("a ticket that was turned away because the endpoint was busy is still good: %d", st)
	}
}

func TestWhatTheEndpointAnswersIsTold5xxFromARefusalByTheSandboxsOwnWords(t *testing.T) {
	for name, tc := range map[string]struct {
		err   error
		state string
		code  string
	}{
		"the store is away": {blob.ErrUnavailable, "queued", "store_unavailable"},
		"an internal error": {errors.New("boom"), "queued", "store_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			c := newConv(t)
			slow := &slowReads{Store: c.e.store, err: tc.err}
			if errors.Is(tc.err, blob.ErrUnavailable) {
				slow.err = errors.New("the original store is not reachable")
				slow.err = &postgres.Error{Code: "NOT_CONFIGURED", Message: "the original store is not reachable"}
			}
			c.handler.Backend = slow
			id := c.stub("bob", "doc", "placeholder")
			c.put("bob", id, mediaPDF, "%PDF TAG")
			var wg sync.WaitGroup
			c.worker.Tick(context.Background(), &wg)
			wg.Wait()
			in := ingestOf(c.doc("bob", id))
			if in["state"] != tc.state || in["error"] != tc.code {
				t.Fatalf("a 5xx of the endpoint is the sandbox's \"(HTTP n)\" and is tried again: %v", in)
			}
		})
	}
	// a refusal with a message is permanent
	c := newConv(t)
	c.handler.Backend = &slowReads{Store: c.e.store, err: &postgres.Error{Code: "INVALID", Message: "the original is no good"}}
	id := c.stub("bob", "doc", "placeholder")
	c.put("bob", id, mediaPDF, "%PDF TAG")
	var wg sync.WaitGroup
	c.worker.Tick(context.Background(), &wg)
	wg.Wait()
	if in := ingestOf(c.doc("bob", id)); in["state"] != "failed" || in["error"] != "import_refused" {
		t.Fatalf("%v", in)
	}
}

func TestPresentedTicketsAreForgottenAfterTheyWouldHaveExpired(t *testing.T) {
	c := newConv(t)
	old := c.running("bob", "a", "%PDF ZqxA")
	fresh := c.running("bob", "b", "%PDF ZqxB")
	for _, r := range []running{old, fresh} {
		if st, _, _ := c.get(c.tok(c.claims(r, ticket.Read))); st != 200 {
			t.Fatal(st)
		}
	}
	ownerExec(t, c.e.ownerURL, `UPDATE knowledge_ticket_uses SET expires_at = now() - interval '3 hours' WHERE document_id = $1`, old.id)
	n, err := c.e.store.PurgeTicketUses(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	var left int
	if err := ownerQuery(t, c.e.ownerURL, `SELECT count(*) FROM knowledge_ticket_uses`, &left); err != nil || left != 1 {
		t.Fatalf("%d %v", left, err)
	}
}

var _ = io.Discard
var _ = cmi.MaxDocument
var _ convert.Gateway

// ADR-0001 D77: a version that is or was in force never changes its source text. A convertible original goes with a version
// that starts pending (awaitsOriginal); the one in-place correction is the same original converted again.
func TestAVersionThatWasInForceKeepsItsText(t *testing.T) {
	c := newConv(t)
	c.output("TAG", convtest.Output{Markdown: "<!-- page: 1 -->\nZqxConverted text.\n"})
	putStatus := func(id, media, body string) int {
		st, _, _ := c.e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", media, []byte(body))
		return st
	}
	real := c.plain("bob", "doc", "ZqxReal text a reader saw")
	time.Sleep(60 * time.Millisecond)
	seen := time.Now() // a view as of this moment returned the real text
	time.Sleep(60 * time.Millisecond)
	if st := putStatus(real, mediaPDF, "%PDF TAG"); st != 409 {
		t.Fatalf("a convertible original on a version that is in force: %d", st)
	}
	// nothing was changed: not the text, not the status, and not what a view of the past returned
	if d := c.doc("bob", real); d["status"] != "active" || d["original"] != nil || d["ingest"] != nil {
		t.Fatalf("%v", d)
	}
	asOf := map[string]any{"recorded": nano(seen)}
	if got := excerpts(readDoc(c.e, "bob", map[string]any{"documentId": real, "purpose": "p", "asOf": asOf})); !strings.Contains(got, "ZqxReal") {
		t.Fatalf("as of the moment it was seen: %q", got)
	}
	if len(c.hits("bob", "ZqxReal")) != 1 {
		t.Fatal("the text of the version in force is searchable")
	}
	// a text original is its own text and is not a change of the text the version was written with
	if st := putStatus(real, "text/markdown", "# notes"); st != 200 {
		t.Fatalf("a markdown original: %d", st)
	}
	// a replaced version and a withdrawn one take none either
	older := c.plain("bob", "two", "ZqxOlder")
	c.plain("bob", "two", "ZqxNewer")
	if st := putStatus(older, mediaPDF, "%PDF TAG"); st != 409 {
		t.Fatalf("a replaced version: %d", st)
	}
	gone := c.plain("bob", "three", "ZqxGone")
	c.e.must(200, "DELETE", "/v1/knowledge/documents/"+gone+"?reason=x", "bob", nil)
	if st := putStatus(gone, mediaPDF, "%PDF TAG"); st != 409 {
		t.Fatalf("a withdrawn version: %d", st)
	}
	// a version written to wait for its original takes it, and is in force from the commit
	wait := c.stub("bob", "four", "placeholder")
	before := time.Now()
	time.Sleep(60 * time.Millisecond)
	if st := putStatus(wait, mediaPDF, "%PDF TAG"); st != 200 {
		t.Fatalf("the original of a version that waits for it: %d", st)
	}
	c.drain()
	if d := c.doc("bob", wait); d["status"] != "active" || ingestOf(d)["state"] != "succeeded" || len(c.hits("bob", "ZqxConverted")) != 1 {
		t.Fatalf("%v", d)
	}
	c.e.must(404, "POST", "/v1/context/read", "bob", map[string]any{"documentId": wait, "purpose": "p", "asOf": map[string]any{"recorded": nano(before)}})
	// the one in-place correction: the same original converted again; another original is a new version
	if st := putStatus(wait, mediaPDF, "%PDF TAG"); st != 200 {
		t.Fatalf("the same original again: %d", st)
	}
	c.drain()
	c.e.must(202, "POST", "/v1/knowledge/documents/"+wait+"/ingest", "bob", nil)
	c.drain()
	if st := putStatus(wait, mediaPDF, "%PDF TAG and more"); st != 409 {
		t.Fatalf("another original on a version that holds converted text: %d", st)
	}
	// a document written to wait needs a service that converts
	c.e.store.Conversion.Enabled = false
	body := kbDoc("member-notes", "kb://me/five", "", "placeholder")
	body["awaitsOriginal"] = true
	c.e.must(503, "POST", "/v1/knowledge/documents", "bob", body)
}
