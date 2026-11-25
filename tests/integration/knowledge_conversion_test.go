package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-cmi/adapters/blob"
	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/convert"
	"idp-cmi/internal/convert/convtest"
	"idp-cmi/internal/ingestfiles"
	"idp-cmi/internal/mcpclient"
	"idp-cmi/internal/ticket"
)

const (
	mediaPDF  = "application/pdf"
	mediaDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	mediaPPTX = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	mediaXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

var ticketKey = []byte("0123456789abcdef0123456789abcdef")

type staticTokens struct{}

func (staticTokens) Token(context.Context) (string, error) { return "workload-token", nil }
func (staticTokens) Invalidate(string)                     {}

// conv is the conversion path in process: CMI's file endpoint, a stand-in for the system gateway and the ingest sandbox, and the worker.
type conv struct {
	t        *testing.T
	e        *env
	mem      *blob.Mem
	sb       *convtest.Sandbox
	files    *httptest.Server
	handler  *ingestfiles.Handler
	signer   *ticket.Signer
	verifier *ticket.Verifier
	worker   *convert.Worker
	mu       sync.Mutex
	outputs  map[string]convtest.Output // by a tag inside the original's bytes
	outcomes []string
}

func newConv(t *testing.T) *conv {
	t.Helper()
	e := newEnv(t)
	kbSources(e)
	c := &conv{t: t, e: e, mem: &blob.Mem{}, outputs: map[string]convtest.Output{}}
	e.store.Originals = c.mem
	e.store.Conversion = postgres.ConversionSettings{Enabled: true, MaxAttempts: 4, Lease: 2 * time.Minute}
	c.handler = &ingestfiles.Handler{Backend: e.store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c.files = httptest.NewServer(c.handler)
	t.Cleanup(c.files.Close)
	c.verifier = &ticket.Verifier{Key: ticketKey, Audience: c.files.URL}
	c.signer = &ticket.Signer{Key: ticketKey, Audience: c.files.URL, TTL: 5 * time.Minute}
	c.handler.Verifier = c.verifier
	c.sb = convtest.New("workload-token", "system", "svc-cmi-ingest")
	t.Cleanup(c.sb.Close)
	c.sb.FilesURL = c.files.URL
	c.sb.Convert = func(cmd, name string, data []byte) convtest.Output {
		c.mu.Lock()
		defer c.mu.Unlock()
		for tag, out := range c.outputs {
			if strings.Contains(string(data), tag) {
				return out
			}
		}
		return convtest.Output{Exit: 4, Stderr: cmd + ": error: corrupt: not a document"}
	}
	gw := &mcpclient.Client{Endpoint: c.sb.URL(), HTTP: mcpclient.NewHTTPClient(20*time.Second, nil), Tokens: staticTokens{}, Info: mcpclient.ClientInfo{Name: "idp-cmi", Version: "it"}}
	c.worker = &convert.Worker{Store: e.store, Gateway: gw, Signer: c.signer, WorkloadTenant: "system", WorkloadSubject: "svc-cmi-ingest", ToolPrefix: "ingest__",
		Owner: "it-1", Concurrency: 4, RunSeconds: 60, Backoff: func(int) time.Duration { return 0 }, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Finished: func(outcome, code string) {
			c.mu.Lock()
			c.outcomes = append(c.outcomes, outcome+":"+code)
			c.mu.Unlock()
		}}
	return c
}

func (c *conv) output(tag string, o convtest.Output) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outputs[tag] = o
}

func (c *conv) Outcomes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.outcomes...)
}

// drain runs the worker until nothing is due.
func (c *conv) drain() {
	c.t.Helper()
	for round := 0; round < 40; round++ {
		var wg sync.WaitGroup
		c.worker.Tick(context.Background(), &wg)
		wg.Wait()
		if !c.pending() {
			return
		}
	}
	c.t.Fatal("the queue did not drain")
}

// pending is whether any job is queued or running (read as the schema owner: the tests look at every tenant).
func (c *conv) pending() bool {
	c.t.Helper()
	var n int
	if err := ownerQuery(c.t, c.e.ownerURL, `SELECT count(*) FROM knowledge_conversions WHERE state IN ('queued', 'running')`, &n); err != nil {
		c.t.Fatal(err)
	}
	return n > 0
}

// stub is a version a member wrote for an original: a placeholder text; the original then arrives.
func (c *conv) stub(who, path, text string) string {
	c.t.Helper()
	d := c.e.must(201, "POST", "/v1/knowledge/documents", who, kbDoc("member-notes", "kb://me/"+path, "", text))
	return d["documentId"].(string)
}

func (c *conv) put(who, id, media, body string) map[string]any {
	c.t.Helper()
	st, _, raw := c.e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", who, media, []byte(body))
	if st != 200 {
		c.t.Fatalf("put original %d %s", st, raw)
	}
	return decodeBody(c.t, raw)
}

func (c *conv) doc(who, id string) map[string]any {
	c.t.Helper()
	return c.e.must(200, "GET", "/v1/knowledge/documents/"+id, who, nil)
}

func ingestOf(d map[string]any) map[string]any {
	m, _ := d["ingest"].(map[string]any)
	return m
}

func (c *conv) hits(who, word string) []map[string]any {
	c.t.Helper()
	var out []map[string]any
	for _, it := range items(c.e.must(200, "POST", "/v1/context/resolve", who, map[string]any{"query": word, "kinds": []string{"knowledge"}})) {
		if strings.Contains(fmt.Sprint(it["excerpt"]), word) {
			out = append(out, it)
		}
	}
	return out
}

func locatorOf(it map[string]any) map[string]any {
	l, _ := it["locator"].(map[string]any)
	return l
}

func TestAPdfOriginalBecomesSearchableTextWithPages(t *testing.T) {
	c := newConv(t)
	old := c.stub("bob", "handbook", "the older handbook says ZqxOldRule applies")
	if len(c.hits("bob", "ZqxOldRule")) != 1 {
		t.Fatal("the first version is searchable")
	}
	id := c.stub("bob", "handbook", "placeholder ZqxStubWord")
	c.output("PDF-HANDBOOK", convtest.Output{Markdown: "<!-- page: 1 -->\nIntroduction ZqxPageOne text.\n<!-- page: 2 -->\n# Rates\nThe ZqxPageTwo rate is 41.50 per kilo.\n<!-- page: 3 -->\n<!-- no-text: page 3 (a picture) -->\n"})
	doc := c.put("bob", id, mediaPDF, "%PDF-1.4 PDF-HANDBOOK")
	if doc["status"] != "pending" || ingestOf(doc)["state"] != "queued" {
		t.Fatalf("a version waiting for its text is pending and queued: %v", doc)
	}
	// while the job has not run the version before it stays current, and the placeholder is not searched
	if len(c.hits("bob", "ZqxOldRule")) != 1 || len(c.hits("bob", "ZqxStubWord")) != 0 {
		t.Fatal("the previous version is current until the conversion commits")
	}
	_ = old
	c.drain()
	doc = c.doc("bob", id)
	in := ingestOf(doc)
	if doc["status"] != "active" || in["state"] != "succeeded" || !strings.Contains(fmt.Sprint(in["notice"]), "1 page(s) have no text layer") {
		t.Fatalf("converted: %v", doc)
	}
	if len(c.hits("bob", "ZqxStubWord")) != 0 || len(c.hits("bob", "ZqxOldRule")) != 0 {
		t.Fatal("the extracted text replaced the placeholder and the older version is superseded")
	}
	two := c.hits("bob", "ZqxPageTwo")
	if len(two) != 1 || locatorOf(two[0])["page"].(float64) != 2 || fmt.Sprint(locatorOf(two[0])["headings"]) != "[Rates]" {
		t.Fatalf("page two %v", two)
	}
	if one := c.hits("bob", "ZqxPageOne"); len(one) != 1 || locatorOf(one[0])["page"].(float64) != 1 {
		t.Fatalf("page one %v", one)
	}
	if got := c.sb.Calls(); strings.Join(got, ",") != "sandbox_create,sandbox_import_file,sandbox_run,sandbox_export_file,sandbox_delete" {
		t.Fatalf("calls %v", got)
	}
	if !strings.Contains(c.sb.Scripts()[0], "pdftext in/original.pdf --markdown out/extract.md") || c.sb.Workspaces() != 0 {
		t.Fatalf("script %q, %d workspaces left", c.sb.Scripts()[0], c.sb.Workspaces())
	}
	if keys := c.mem.Keys(); len(keys) != 1 {
		t.Fatalf("only the original is left in the store, not the staged text: %v", keys)
	}
	// the vectors follow through the projection
	if n, err := c.e.store.Project(context.Background(), 100); err != nil || n == 0 {
		t.Fatalf("projected %d %v", n, err)
	}
	var embedded int
	if err := ownerQuery(t, c.e.ownerURL, `SELECT count(*) FROM knowledge_chunks WHERE document_id = $1 AND embedding IS NOT NULL`, &embedded, id); err != nil || embedded == 0 {
		t.Fatalf("embedded %d %v", embedded, err)
	}
	if auditCount(c.e, "knowledge.ingest.queued", id) != 1 || auditCount(c.e, "knowledge.ingest.succeeded", id) != 1 {
		t.Fatal("the ingest is audited")
	}
}

func TestOfficeOriginalsAreLocatedBySlideHeadingAndCells(t *testing.T) {
	c := newConv(t)
	c.output("DECK", convtest.Output{Markdown: "<!-- slide: 1 -->\n# Plan\nBudget ZqxSlideOne words.\n<!-- slide: 2 -->\n# Risks\nRisk ZqxSlideTwo words.\n<!-- notes: slide 2 -->\nSpeaker ZqxNotes words.\n"})
	c.output("WORD", convtest.Output{Markdown: "# Policy\n## Travel\nBook ZqxTravel early.\n<!-- table: 2 rows x 2 cols -->\n| Item | Cap |\n| --- | --- |\n| Hotel | 600 |\n"})
	var rows strings.Builder
	for r := 2; r <= 300; r++ {
		fmt.Fprintf(&rows, "| %d | ZqxCell%d | %d |\n", r, r, r*3)
	}
	c.output("SHEET", convtest.Output{Markdown: "<!-- sheet: Rates 2026 range: A1:C300 -->\n| row | A | B | C |\n| --- | --- | --- | --- |\n" + rows.String() +
		"<!-- truncated: sheet Rates 2026, rows kept 10000 of 120000 -->\n"})
	deck := c.stub("bob", "deck", "placeholder")
	word := c.stub("bob", "policy", "placeholder")
	sheet := c.stub("bob", "rates", "placeholder")
	c.put("bob", deck, mediaPPTX, "PK\x03\x04 DECK")
	c.put("bob", word, mediaDOCX, "PK\x03\x04 WORD")
	c.put("bob", sheet, mediaXLSX, "PK\x03\x04 SHEET")
	c.drain()
	if hs := c.hits("bob", "ZqxSlideTwo"); len(hs) != 1 || locatorOf(hs[0])["slide"].(float64) != 2 || fmt.Sprint(locatorOf(hs[0])["headings"]) != "[Risks]" {
		t.Fatalf("slide %v", hs)
	}
	if len(c.hits("bob", "ZqxNotes")) != 1 {
		t.Fatal("the notes of a slide are text of the slide")
	}
	if hs := c.hits("bob", "ZqxTravel"); len(hs) != 1 || fmt.Sprint(locatorOf(hs[0])["headings"]) != "[Policy Travel]" {
		t.Fatalf("heading path %v", hs)
	}
	hs := c.hits("bob", "ZqxCell137")
	if len(hs) != 1 {
		t.Fatalf("cell %v", hs)
	}
	l := locatorOf(hs[0])
	var from, to int
	if n, err := fmt.Sscanf(fmt.Sprint(l["range"]), "A%d:C%d", &from, &to); n != 2 || err != nil || from > 137 || to < 137 || l["sheet"] != "Rates 2026" {
		t.Fatalf("the chunk with row 137 is located by sheet and the cells it covers: %v", l)
	}
	for _, want := range []string{"truncated"} {
		if in := ingestOf(c.doc("bob", sheet)); in["state"] != want || !strings.Contains(fmt.Sprint(in["notice"]), "rows kept 10000 of 120000") {
			t.Fatalf("a worksheet over the cap ingests as truncated and tells the uploader: %v", in)
		}
	}
	if in := ingestOf(c.doc("bob", deck)); in["state"] != "succeeded" {
		t.Fatalf("deck %v", in)
	}
	scripts := strings.Join(c.sb.Scripts(), "\n")
	for _, want := range []string{"ooxml2md in/original.pptx -o out/extract.md", "ooxml2md in/original.docx -o out/extract.md", "ooxml2md in/original.xlsx -o out/extract.md --max-rows 10000 --max-cols 200"} {
		if !strings.Contains(scripts, want) {
			t.Fatalf("no %q in %s", want, scripts)
		}
	}
}

func TestConversionErrorsArePermanentAndLeaveTheOldVersionCurrent(t *testing.T) {
	cases := []struct {
		name string
		out  convtest.Output
		code string
	}{
		{"corrupt", convtest.Output{Exit: 4, Stderr: "pdftext: error: corrupt: no xref"}, "corrupt"},
		{"unsupported", convtest.Output{Exit: 3, Stderr: "pdftext: error: unsupported: password"}, "unsupported"},
		{"limits", convtest.Output{Exit: 5, Stderr: "ooxml2md: error: limits: output too large"}, "limits"},
		{"scan", convtest.Output{Exit: 6, Stderr: "pdftext: error: empty: 12 pages, none has a text layer"}, "no_text"},
		{"timeout", convtest.Output{Exit: 137, TimedOut: true}, "conversion_timeout"},
		{"oom with a zero exit", convtest.Output{Exit: 0, Stderr: "System.OutOfMemoryException: Array dimensions exceeded"}, "conversion_oom"},
		{"an empty file with a good exit", convtest.Output{Markdown: "<!-- page: 1 -->\n<!-- no-text: page 1 -->\n\n"}, "empty_output"},
		{"a usage error", convtest.Output{Exit: 2, Stderr: "pdftext: error: usage"}, "converter_usage"},
		{"another exit code", convtest.Output{Exit: 99}, "conversion_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newConv(t)
			c.stub("bob", "doc", "the older ZqxKept version")
			id := c.stub("bob", "doc", "placeholder")
			c.output("TAG", tc.out)
			c.put("bob", id, mediaPDF, "%PDF TAG")
			c.drain()
			d := c.doc("bob", id)
			in := ingestOf(d)
			if in["state"] != "failed" || in["error"] != tc.code || in["attempts"].(float64) != 1 {
				t.Fatalf("%v", d)
			}
			if d["status"] != "pending" || len(c.hits("bob", "ZqxKept")) != 1 {
				t.Fatalf("nothing half-made is searchable and the older version stays current: %v", d)
			}
			if c.sb.Workspaces() != 0 || len(c.mem.Keys()) != 1 {
				t.Fatalf("workspaces %d, objects %v", c.sb.Workspaces(), c.mem.Keys())
			}
			if auditCount(c.e, "knowledge.ingest.failed", id) != 1 {
				t.Fatal("the failure is audited")
			}
		})
	}
}

func TestATransientConverterErrorIsTriedOnceMore(t *testing.T) {
	c := newConv(t)
	id := c.stub("bob", "doc", "placeholder")
	c.output("TAG", convtest.Output{Exit: 1, Stderr: "pdftext: error: internal: boom"})
	c.put("bob", id, mediaPDF, "%PDF TAG")
	c.drain()
	in := ingestOf(c.doc("bob", id))
	if in["state"] != "failed" || in["error"] != "converter_internal" || in["attempts"].(float64) != 2 {
		t.Fatalf("an internal error of the converter gets two attempts: %v", in)
	}
}

func TestBusySandboxAndGatewayTroublesAreRetried(t *testing.T) {
	c := newConv(t)
	var mu sync.Mutex
	script := []*convtest.Reply{
		{IsError: true, Text: "the sandbox is at capacity; try again later"}, // the 9th run
		{Status: 502}, // the gateway
		{IsError: true, Text: "you already hold the maximum number of workspaces; delete one first"},
	}
	c.sb.Before = func(tool string, _ map[string]any) *convtest.Reply {
		mu.Lock()
		defer mu.Unlock()
		if tool == "sandbox_create" && len(script) > 0 {
			r := script[0]
			script = script[1:]
			return r
		}
		return nil
	}
	c.output("TAG", convtest.Output{Markdown: "<!-- page: 1 -->\nZqxRetried text.\n"})
	id := c.stub("bob", "doc", "placeholder")
	c.put("bob", id, mediaPDF, "%PDF TAG")
	c.drain()
	in := ingestOf(c.doc("bob", id))
	if in["state"] != "succeeded" || in["attempts"].(float64) != 4 {
		t.Fatalf("three transient failures, then success: %v", in)
	}
	if got := strings.Join(c.Outcomes(), " "); got != "retry:sandbox_busy retry:gateway_unavailable retry:sandbox_busy succeeded:" {
		t.Fatalf("outcomes %s", got)
	}
}

func TestTransientFailuresEndInFailureAfterTheAttempts(t *testing.T) {
	c := newConv(t)
	c.sb.Before = func(tool string, _ map[string]any) *convtest.Reply {
		if tool == "sandbox_create" {
			return &convtest.Reply{Status: 503}
		}
		return nil
	}
	id := c.stub("bob", "doc", "placeholder")
	c.put("bob", id, mediaPDF, "%PDF TAG")
	c.drain()
	in := ingestOf(c.doc("bob", id))
	if in["state"] != "failed" || in["error"] != "gateway_unavailable" || in["attempts"].(float64) != 4 {
		t.Fatalf("%v", in)
	}
}

func TestAGatewayTimeoutIsTransient(t *testing.T) {
	c := newConv(t)
	c.worker.Gateway = &mcpclient.Client{Endpoint: c.sb.URL(), HTTP: mcpclient.NewHTTPClient(150*time.Millisecond, nil), Tokens: staticTokens{}}
	c.sb.Before = func(tool string, _ map[string]any) *convtest.Reply {
		if tool == "sandbox_run" {
			return &convtest.Reply{Hang: 400 * time.Millisecond}
		}
		return nil
	}
	c.output("TAG", convtest.Output{Markdown: "<!-- page: 1 -->\nZqxLate text.\n"})
	id := c.stub("bob", "doc", "placeholder")
	c.put("bob", id, mediaPDF, "%PDF TAG")
	var wg sync.WaitGroup
	c.worker.Tick(context.Background(), &wg)
	wg.Wait()
	in := ingestOf(c.doc("bob", id))
	if in["state"] != "queued" || in["error"] != "gateway_timeout" || in["attempts"].(float64) != 1 {
		t.Fatalf("a call that timed out (the gateway's 60 s) is retried: %v", in)
	}
	if c.sb.Workspaces() != 0 {
		t.Fatalf("the workspace of the timed out attempt is deleted: %d", c.sb.Workspaces())
	}
}

func TestConcurrencyStaysWithinTheSandboxAndTheNinthRunIsRetried(t *testing.T) {
	c := newConv(t)
	c.worker.Concurrency = 6
	c.sb.MaxRuns = 3 // the sandbox is smaller than the worker: the extra runs are refused outright
	c.output("TAG", convtest.Output{Markdown: "<!-- page: 1 -->\nZqxMany text.\n", Delay: 60 * time.Millisecond})
	var ids []string
	for i := 0; i < 8; i++ {
		id := c.stub("bob", fmt.Sprintf("doc-%d", i), "placeholder")
		c.put("bob", id, mediaPDF, "%PDF TAG")
		ids = append(ids, id)
	}
	c.worker.Backoff = func(int) time.Duration { return 0 }
	c.e.store.Conversion.MaxAttempts = 50
	c.drain()
	for _, id := range ids {
		if in := ingestOf(c.doc("bob", id)); in["state"] != "succeeded" {
			t.Fatalf("%v", in)
		}
	}
	if m := c.sb.MaxConcurrentRuns(); m > 3 {
		t.Fatalf("the sandbox ran %d at once", m)
	}
	if c.sb.Workspaces() != 0 {
		t.Fatal("every workspace is deleted")
	}
}

func TestANewOriginalWhileTheOldOneIsBeingConvertedWins(t *testing.T) {
	c := newConv(t)
	id := c.stub("bob", "doc", "placeholder")
	c.output("FIRST", convtest.Output{Markdown: "<!-- page: 1 -->\nZqxFirstOriginal text.\n"})
	c.output("SECOND", convtest.Output{Markdown: "<!-- page: 1 -->\nZqxSecondOriginal text.\n"})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.sb.Before = func(tool string, _ map[string]any) *convtest.Reply {
		if tool == "sandbox_run" {
			once.Do(func() { close(entered); <-release })
		}
		return nil
	}
	c.put("bob", id, mediaPDF, "%PDF FIRST")
	var wg sync.WaitGroup
	c.worker.Tick(context.Background(), &wg)
	<-entered
	c.put("bob", id, mediaPDF, "%PDF SECOND") // the first attempt is mid-run
	close(release)
	wg.Wait()
	if len(c.hits("bob", "ZqxFirstOriginal")) != 0 {
		t.Fatal("the text of a replaced original is never committed")
	}
	c.drain()
	if len(c.hits("bob", "ZqxSecondOriginal")) != 1 || len(c.hits("bob", "ZqxFirstOriginal")) != 0 {
		t.Fatal("the newer original's text is what is searched")
	}
	if len(c.mem.Keys()) != 1 {
		t.Fatalf("one original and no staged text are left: %v", c.mem.Keys())
	}
}

func TestAJobOfADeadWorkerIsTakenAgain(t *testing.T) {
	c := newConv(t)
	c.e.store.Conversion.Lease = time.Second
	c.output("TAG", convtest.Output{Markdown: "<!-- page: 1 -->\nZqxRevived text.\n"})
	id := c.stub("bob", "doc", "placeholder")
	c.put("bob", id, mediaPDF, "%PDF TAG")
	jobs, err := c.e.store.ClaimConversions(context.Background(), "dead-replica", 5)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("%v %v", jobs, err)
	}
	if again, _ := c.e.store.ClaimConversions(context.Background(), "other", 5); len(again) != 0 {
		t.Fatal("a job under a lease is not taken")
	}
	time.Sleep(1200 * time.Millisecond)
	c.drain()
	in := ingestOf(c.doc("bob", id))
	if in["state"] != "succeeded" || in["attempts"].(float64) != 2 {
		t.Fatalf("%v", in)
	}
}

func TestAConversionOverTheQuotaFailsWithoutCommitting(t *testing.T) {
	c := newConv(t)
	c.e.must(201, "POST", "/api/v1/knowledge/bases", "alice", kbBase("small", "tenant", "", "handbook", map[string]any{"roles": []string{"cmi-admin"}}, 0, 400))
	c.output("TAG", convtest.Output{Markdown: "<!-- page: 1 -->\n" + strings.Repeat("ZqxBig words in a long text. ", 60)})
	d := c.e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "kb://small/big", "small", "placeholder"))
	id := d["documentId"].(string)
	st, _, raw := c.e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "alice", mediaPDF, []byte("%PDF TAG"))
	if st != 200 {
		t.Fatalf("%d %s", st, raw)
	}
	c.drain()
	in := ingestOf(c.e.must(200, "GET", "/api/v1/knowledge/documents/"+id, "alice", nil))
	if in["state"] != "failed" || in["error"] != "quota_exceeded" {
		t.Fatalf("%v", in)
	}
}

func TestWithoutConversionOriginalsAreOnlyStored(t *testing.T) {
	c := newConv(t)
	c.e.store.Conversion.Enabled = false
	id := c.stub("bob", "doc", "the text ZqxStays as written")
	doc := c.put("bob", id, mediaPDF, "%PDF anything")
	if doc["status"] != "active" || doc["ingest"] != nil {
		t.Fatalf("%v", doc)
	}
	if len(c.hits("bob", "ZqxStays")) != 1 {
		t.Fatal("the text is searchable as before")
	}
}

func TestAMarkdownOriginalIsNotConverted(t *testing.T) {
	c := newConv(t)
	id := c.stub("bob", "doc", "the notes ZqxNotes text")
	doc := c.put("bob", id, "text/markdown", "# notes")
	if doc["status"] != "active" || doc["ingest"] != nil {
		t.Fatalf("a text original is its own text: %v", doc)
	}
}

func TestRetryingAFailedConversion(t *testing.T) {
	c := newConv(t)
	id := c.stub("bob", "doc", "placeholder")
	c.output("TAG", convtest.Output{Exit: 4, Stderr: "pdftext: error: corrupt"})
	c.put("bob", id, mediaPDF, "%PDF TAG")
	c.drain()
	if ingestOf(c.doc("bob", id))["state"] != "failed" {
		t.Fatal("setup")
	}
	c.e.must(404, "POST", "/v1/knowledge/documents/"+id+"/ingest", "carol", nil) // someone else's personal base is not there for her
	c.output("TAG", convtest.Output{Markdown: "<!-- page: 1 -->\nZqxSecondTry text.\n"})
	d := c.e.must(202, "POST", "/v1/knowledge/documents/"+id+"/ingest", "bob", nil)
	if ingestOf(d)["state"] != "queued" {
		t.Fatalf("%v", d)
	}
	c.e.must(409, "POST", "/v1/knowledge/documents/"+id+"/ingest", "bob", nil) // one at a time
	c.drain()
	if d := c.doc("bob", id); d["status"] != "active" || ingestOf(d)["state"] != "succeeded" || len(c.hits("bob", "ZqxSecondTry")) != 1 {
		t.Fatalf("%v", d)
	}
	// a text original has nothing to convert
	md := c.stub("bob", "notes", "plain")
	c.put("bob", md, "text/markdown", "# x")
	c.e.must(422, "POST", "/v1/knowledge/documents/"+md+"/ingest", "bob", nil)
}

func TestTheWorkerRunsNoMoreConversionsThanItIsToldTo(t *testing.T) {
	c := newConv(t)
	c.worker.Concurrency = 2
	c.output("TAG", convtest.Output{Markdown: "<!-- page: 1 -->\nZqxFew text.\n", Delay: 300 * time.Millisecond})
	for i := 0; i < 6; i++ {
		id := c.stub("bob", fmt.Sprintf("doc-%d", i), "placeholder")
		c.put("bob", id, mediaPDF, "%PDF TAG")
	}
	// the queue is looked at again and again while the first conversions still run, as Run does
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		c.worker.Tick(context.Background(), &wg)
		time.Sleep(40 * time.Millisecond)
	}
	wg.Wait()
	c.drain()
	if m := c.sb.MaxConcurrentRuns(); m > 2 || m == 0 {
		t.Fatalf("the sandbox ran %d at once; the worker was told 2", m)
	}
}

func TestATextOverTheLimitOfADocumentIsAFailure(t *testing.T) {
	c := newConv(t)
	c.output("TAG", convtest.Output{Markdown: "<!-- page: 1 -->\n" + strings.Repeat("word ", 900_000) + "\n"}) // 4.5 MB: over the 4 MiB of a document, under what the endpoint takes
	id := c.stub("bob", "doc", "placeholder")
	c.put("bob", id, mediaPDF, "%PDF TAG")
	c.drain()
	if in := ingestOf(c.doc("bob", id)); in["state"] != "failed" || in["error"] != "text_too_large" {
		t.Fatalf("%v", in)
	}
}

func TestAWorkerThatLostItsLeaseCannotCommit(t *testing.T) {
	c := newConv(t)
	c.e.store.Conversion.Lease = time.Second
	id := c.stub("bob", "doc", "placeholder")
	c.put("bob", id, mediaPDF, "%PDF TAG")
	first, err := c.e.store.ClaimConversions(context.Background(), "slow-replica", 5)
	if err != nil || len(first) != 1 {
		t.Fatalf("%v %v", first, err)
	}
	time.Sleep(1200 * time.Millisecond)
	second, err := c.e.store.ClaimConversions(context.Background(), "other-replica", 5)
	if err != nil || len(second) != 1 || second[0].Attempts != 2 {
		t.Fatalf("%v %v", second, err)
	}
	_, err = c.e.store.CommitConversion(context.Background(), first[0], "<!-- page: 1 -->\nZqxStale text", cmiInfo())
	if !errors.Is(err, postgres.ErrConversionLost) {
		t.Fatalf("the first attempt committed: %v", err)
	}
	if len(c.hits("bob", "ZqxStale")) != 0 {
		t.Fatal("nothing of it is searchable")
	}
	if _, err := c.e.store.CommitConversion(context.Background(), second[0], "<!-- page: 1 -->\nZqxCurrent text", cmiInfo()); err != nil {
		t.Fatal(err)
	}
	if len(c.hits("bob", "ZqxCurrent")) != 1 {
		t.Fatal("the current attempt commits")
	}
}

func TestAPendingVersionCanBeWithdrawnAndItsConversionThenCommitsNothing(t *testing.T) {
	c := newConv(t)
	c.output("TAG", convtest.Output{Markdown: "<!-- page: 1 -->\nZqxWithdrawn text.\n"})
	id := c.stub("bob", "doc", "placeholder")
	c.put("bob", id, mediaPDF, "%PDF TAG")
	c.e.must(200, "DELETE", "/v1/knowledge/documents/"+id+"?reason=wrong+file", "bob", nil)
	c.drain()
	if d := c.doc("bob", id); d["status"] != "revoked" || len(c.hits("bob", "ZqxWithdrawn")) != 0 {
		t.Fatalf("a withdrawn version stays withdrawn: %v", d)
	}
	if in := ingestOf(c.doc("bob", id)); in["state"] != "failed" || in["error"] != "document_changed" || c.sb.Count("sandbox_create") != 0 {
		t.Fatalf("a conversion nobody wants is ended before the sandbox is asked anything: %v, %d workspaces made", in, c.sb.Count("sandbox_create"))
	}
}
