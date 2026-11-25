package integration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"idp-cmi/internal/convert/convtest"
)

// leftOf counts what an erased document may not leave behind: jobs, presented tickets, chunks with text or a locator, objects.
func (c *conv) leftOf(ids ...string) (jobs, tickets, chunks, located int) {
	c.t.Helper()
	q := func(sql string, dest *int) {
		if err := ownerQuery(c.t, c.e.ownerURL, sql, dest, ids); err != nil {
			c.t.Fatal(err)
		}
	}
	q(`SELECT count(*) FROM knowledge_conversions WHERE document_id = ANY($1::uuid[])`, &jobs)
	q(`SELECT count(*) FROM knowledge_ticket_uses WHERE document_id = ANY($1::uuid[])`, &tickets)
	q(`SELECT count(*) FROM knowledge_chunks WHERE document_id = ANY($1::uuid[]) AND (content <> '[erased]' OR index_text <> '[erased]')`, &chunks)
	q(`SELECT count(*) FROM knowledge_chunks WHERE document_id = ANY($1::uuid[]) AND (sheet <> '' OR cell_range <> '' OR heading_path <> '' OR table_name <> '')`, &located)
	return
}

func TestErasingAConvertedDocumentLeavesNoTextJobsTicketsOrObjects(t *testing.T) {
	c := newConv(t)
	c.output("SHEET", convtest.Output{Markdown: "<!-- sheet: ZqxSecretSheet range: A1:B3 -->\n| row | A | B |\n| --- | --- | --- |\n| 2 | ZqxSecretCell | 7 |\n<!-- truncated: sheet ZqxSecretSheet, rows kept 10000 of 20000 -->\n"})
	id := c.stub("bob", "rates", "placeholder")
	c.put("bob", id, mediaXLSX, "PK\x03\x04 SHEET")
	c.drain()
	if len(c.hits("bob", "ZqxSecretCell")) != 1 {
		t.Fatal("setup: converted")
	}
	if jobs, tickets, _, _ := c.leftOf(id); jobs != 1 || tickets != 2 {
		t.Fatalf("setup: %d jobs, %d tickets presented", jobs, tickets)
	}
	er := c.e.must(201, "POST", "/api/v1/erasures", "alice", map[string]any{"documentIds": []string{id}, "reason": "legal"})
	if _, err := c.e.store.Purge(context.Background(), c.e.tenantA, 5); err != nil {
		t.Fatal(err)
	}
	if got := c.e.must(200, "GET", "/api/v1/erasures/"+er["erasureId"].(string), "alice", nil); got["status"] != "verified" {
		t.Fatalf("%v", got)
	}
	if jobs, tickets, chunks, located := c.leftOf(id); jobs+tickets+chunks+located != 0 {
		t.Fatalf("left: %d jobs, %d tickets, %d chunks with text, %d with a locator", jobs, tickets, chunks, located)
	}
	if len(c.mem.Keys()) != 0 || len(c.hits("bob", "ZqxSecretCell")) != 0 {
		t.Fatalf("objects %v", c.mem.Keys())
	}
	var detail string
	if err := ownerQuery(t, c.e.ownerURL, `SELECT string_agg(detail::text, ' ') FROM audit_events WHERE resource_id = $1 AND action LIKE 'knowledge.ingest.%'`, &detail, id); err != nil || strings.Contains(detail, "ZqxSecretSheet") {
		t.Fatalf("the audit of the conversion quotes the document: %s %v", detail, err)
	}
}

func TestErasingAMemberCoversTheirConversionsInFlight(t *testing.T) {
	c := newConv(t)
	c.output("TAG", convtest.Output{Markdown: "<!-- page: 1 -->\nZqxInFlight text.\n"})
	queued := c.stub("bob", "queued", "placeholder")
	c.put("bob", queued, mediaPDF, "%PDF TAG")
	running := c.running("bob", "running", "%PDF TAG")
	staged := c.running("bob", "staged", "%PDF TAG")
	if st, _ := c.post(c.tok(c.claims(staged, "write")), []byte("<!-- page: 1 -->\nZqxStagedText\n")); st != 200 {
		t.Fatalf("staged %d", st)
	}
	if len(c.mem.Keys()) != 4 {
		t.Fatalf("three originals and one staged text: %v", c.mem.Keys())
	}
	er := c.e.must(201, "POST", "/api/v1/erasures", "alice", map[string]any{"subject": "bob", "reason": "left"})
	if _, err := c.e.store.Purge(context.Background(), c.e.tenantA, 5); err != nil {
		t.Fatal(err)
	}
	if got := c.e.must(200, "GET", "/api/v1/erasures/"+er["erasureId"].(string), "alice", nil); got["status"] != "verified" {
		t.Fatalf("%v", got)
	}
	if jobs, tickets, chunks, located := c.leftOf(queued, running.id, staged.id); jobs+tickets+chunks+located != 0 {
		t.Fatalf("left: %d jobs, %d tickets, %d chunks, %d located", jobs, tickets, chunks, located)
	}
	if len(c.mem.Keys()) != 0 {
		t.Fatalf("objects remain: %v", c.mem.Keys())
	}
	c.drain() // nothing is claimed, nothing is committed
	if len(c.hits("bob", "ZqxInFlight")) != 0 {
		t.Fatal("erased")
	}
}

func TestAnErasureDuringARunCommitsNothingAndLeavesNothing(t *testing.T) {
	c := newConv(t)
	c.output("TAG", convtest.Output{Markdown: "<!-- page: 1 -->\nZqxRace text.\n"})
	id := c.stub("bob", "doc", "placeholder")
	c.put("bob", id, mediaPDF, "%PDF TAG")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.sb.Before = func(tool string, _ map[string]any) *convtest.Reply {
		if tool == "sandbox_export_file" {
			once.Do(func() { close(entered); <-release })
		}
		return nil
	}
	var wg sync.WaitGroup
	c.worker.Tick(context.Background(), &wg)
	<-entered // the converter has run; the export is about to present the write ticket
	c.e.must(201, "POST", "/api/v1/erasures", "alice", map[string]any{"documentIds": []string{id}, "reason": "legal"})
	close(release)
	wg.Wait()
	if _, err := c.e.store.Purge(context.Background(), c.e.tenantA, 5); err != nil {
		t.Fatal(err)
	}
	if jobs, tickets, chunks, located := c.leftOf(id); jobs+tickets+chunks+located != 0 || len(c.mem.Keys()) != 0 {
		t.Fatalf("left: %d jobs, %d tickets, %d chunks, %d located, objects %v", jobs, tickets, chunks, located, c.mem.Keys())
	}
	if c.sb.Workspaces() != 0 || len(c.hits("bob", "ZqxRace")) != 0 {
		t.Fatalf("workspaces %d", c.sb.Workspaces())
	}
	for _, o := range c.Outcomes() {
		if strings.HasPrefix(o, "succeeded") || strings.HasPrefix(o, "truncated") {
			t.Fatal(fmt.Sprint("committed: ", c.Outcomes()))
		}
	}
}
