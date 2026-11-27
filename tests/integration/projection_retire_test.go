package integration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/adapters/store/postgres"
)

// D79: an erasure retires the queued projection events of the objects it erases, in its own transaction.

func retireOwner(e *env, t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), e.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// eventState counts the unprocessed events of the objects and those retired with the reason.
func eventState(t *testing.T, conn *pgx.Conn, ids ...string) (open, retired, other int) {
	t.Helper()
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE processed_at IS NULL), count(*) FILTER (WHERE processed_at IS NOT NULL AND processed_reason = 'erased'),
		count(*) FILTER (WHERE processed_at IS NOT NULL AND processed_reason IS DISTINCT FROM 'erased') FROM projection_events WHERE object_id = ANY($1::uuid[])`, ids).Scan(&open, &retired, &other); err != nil {
		t.Fatal(err)
	}
	return
}

func retireSetup(e *env) {
	kbSources(e)
	e.must(201, "POST", "/api/v1/sources", "alice", map[string]any{"name": "assistant", "displayName": "Assistant", "kind": "agent", "trust": "informational"})
}

func longText(word string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%s paragraph %d says something quite different about topic %d.\n\n", word, i, i*7)
	}
	return b.String()
}

func TestErasingADocumentRetiresItsQueuedEventsOnly(t *testing.T) {
	e := newEnv(t)
	retireSetup(e)
	conn := retireOwner(e, t)
	a := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://a", "", longText("retirea", 40)))["documentId"].(string)
	b := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://b", "", longText("retireb", 40)))["documentId"].(string)
	open, _, _ := eventState(t, conn, a)
	openB, _, _ := eventState(t, conn, b)
	if open < 2 || openB < 2 {
		t.Fatalf("setup: %d and %d queued chunk events", open, openB)
	}
	before, err := e.store.ProjectionBacklog(context.Background(), e.tenantA)
	if err != nil {
		t.Fatal(err)
	}
	e.must(201, "POST", "/api/v1/erasures", "alice", map[string]any{"documentIds": []string{a}, "reason": "retire"})
	if o, r, x := eventState(t, conn, a); o != 0 || r != open || x != 0 {
		t.Fatalf("document a: %d open, %d retired (want %d), %d processed otherwise", o, r, open, x)
	}
	if o, r, _ := eventState(t, conn, b); o != openB || r != 0 {
		t.Fatalf("another document's events were touched: %d open, %d retired", o, r)
	}
	// the backlog and the lag do not count what was retired
	after, err := e.store.ProjectionBacklog(context.Background(), e.tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if after.ByKind["chunk"] != before.ByKind["chunk"]-open || after.Pending != before.Pending-open {
		t.Fatalf("backlog %+v before %+v", after, before)
	}
	// what is left is the other document's, and projecting it works as before
	if n, err := e.store.Project(context.Background(), 500); err != nil || n != after.Pending {
		t.Fatalf("project %d %v", n, err)
	}
}

func TestBacklogAndLagIgnoreRetiredEvents(t *testing.T) {
	e := newEnv(t)
	retireSetup(e)
	ctx := context.Background()
	a := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://only", "", longText("lagonly", 10)))["documentId"].(string)
	conn := retireOwner(e, t)
	if _, err := conn.Exec(ctx, `UPDATE projection_events SET created_at = now() - interval '1 hour' WHERE object_id = $1`, a); err != nil {
		t.Fatal(err)
	}
	if lag, _ := e.store.ProjectionLag(ctx, e.tenantA); lag < 3000 {
		t.Fatalf("setup lag %v", lag)
	}
	e.must(201, "POST", "/api/v1/erasures", "alice", map[string]any{"documentIds": []string{a}, "reason": "retire"})
	lag, err := e.store.ProjectionLag(ctx, e.tenantA)
	st, err2 := e.store.ProjectionBacklog(ctx, e.tenantA)
	if err != nil || err2 != nil || lag != 0 || st.Pending != 0 || st.OldestAgeSeconds != 0 || len(st.ByKind) != 0 {
		t.Fatalf("lag %v backlog %+v %v %v", lag, st, err, err2)
	}
}

func TestSubjectErasureRetiresMemoryAndChunkEvents(t *testing.T) {
	e := newEnv(t)
	retireSetup(e)
	conn := retireOwner(e, t)
	var mine, others []string
	for i := 0; i < 3; i++ {
		mine = append(mine, e.must(201, "POST", "/v1/memory/records", "bob", memory("assistant", fmt.Sprintf("bob%d", i), "subject", "bob", fmt.Sprintf("bobnote %d about parcels", i), "internal", e.now))["recordId"].(string))
	}
	others = append(others, e.must(201, "POST", "/v1/memory/records", "bob", memory("assistant", "carol1", "subject", "carol", "carolnote about parcels", "internal", e.now))["recordId"].(string))
	mine = append(mine, e.must(201, "POST", "/v1/knowledge/documents", "bob", kbDoc("member-notes", "kb://me/x", "", longText("bobdoc", 30)))["documentId"].(string))
	others = append(others, e.must(201, "POST", "/v1/knowledge/documents", "carol", kbDoc("member-notes", "kb://me/y", "", longText("caroldoc", 30)))["documentId"].(string))
	o, _, _ := eventState(t, conn, mine...)
	oo, _, _ := eventState(t, conn, others...)
	if o < 5 || oo < 3 {
		t.Fatalf("setup %d %d", o, oo)
	}
	e.must(201, "POST", "/api/v1/erasures", "alice", map[string]any{"subject": "bob", "reason": "left"})
	if open, retired, other := eventState(t, conn, mine...); open != 0 || retired != o || other != 0 {
		t.Fatalf("bob's events: %d open, %d retired (want %d), %d other", open, retired, o, other)
	}
	if open, retired, _ := eventState(t, conn, others...); open != oo || retired != 0 {
		t.Fatalf("carol's events: %d open %d retired", open, retired)
	}
}

func TestRetiringIsIdempotentAndCatchesWhatArrivesBeforeThePurge(t *testing.T) {
	e := newEnv(t)
	retireSetup(e)
	ctx := context.Background()
	conn := retireOwner(e, t)
	a := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://late", "", longText("lateevent", 10)))["documentId"].(string)
	other := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://keep", "", longText("keepevent", 10)))["documentId"].(string)
	e.must(201, "POST", "/api/v1/erasures", "alice", map[string]any{"documentIds": []string{a}, "reason": "retire"})
	_, retired, _ := eventState(t, conn, a)
	// an event a writer queued between the barrier and the purge (a transaction that started before it)
	if _, err := conn.Exec(ctx, `INSERT INTO projection_events (tenant_id, kind, object_id, seq, version, action) VALUES ($1, 'chunk', $2, 99, 1, 'upsert')`, e.tenantA, a); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // the purge retires it, and a second run changes nothing
		if _, err := e.store.Purge(ctx, e.tenantA, 5); err != nil {
			t.Fatal(err)
		}
		if open, r, x := eventState(t, conn, a); open != 0 || r != retired+1 || x != 0 {
			t.Fatalf("run %d: %d open, %d retired (want %d), %d other", i, open, r, retired+1, x)
		}
	}
	if open, r, _ := eventState(t, conn, other); open == 0 || r != 0 {
		t.Fatalf("the other document: %d open %d retired", open, r)
	}
	// an event that was already processed keeps its reason (none)
	var withReason int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM projection_events WHERE object_id = $1 AND processed_reason IS NOT NULL AND processed_at IS NULL`, a).Scan(&withReason); err != nil || withReason != 0 {
		t.Fatal(withReason, err)
	}
}

// blockingEmbedder blocks every call whose text holds the marker until released, and says when one has started.
type blockingEmbedder struct {
	inner interface {
		Embed(ctx context.Context, texts []string) ([][]float32, error)
		Model() string
	}
	marker  string
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingEmbedder) Model() string { return b.inner.Model() }
func (b *blockingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	for _, s := range texts {
		if strings.Contains(s, b.marker) {
			b.once.Do(func() { close(b.started) })
			select {
			case <-b.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return b.inner.Embed(ctx, texts)
}

func TestASlowBackfillDoesNotDelayProjection(t *testing.T) {
	e := newEnv(t)
	retireSetup(e)
	ctx := context.Background()
	conn := retireOwner(e, t)
	// a row without a vector, whose event is done: only the backfill will take it
	e.store.Embedder = nil
	e.must(201, "POST", "/v1/memory/records", "bob", memory("assistant", "slow", "tenant", "", "slowmarker row that the backfill must embed", "internal", e.now))
	if _, err := conn.Exec(ctx, `UPDATE projection_events SET processed_at = now()`); err != nil {
		t.Fatal(err)
	}
	slow := &blockingEmbedder{inner: e.embedder, marker: "slowmarker", started: make(chan struct{}), release: make(chan struct{})}
	e.store.Embedder = slow
	wctx, stop := context.WithCancel(ctx)
	var releaseOnce sync.Once
	t.Cleanup(func() { stop(); releaseOnce.Do(func() { close(slow.release) }) }) // a failure must not leave the loops stuck
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.store.RunEmbeddingWorkers(wctx, postgres.EmbeddingWorkers{Interval: 20 * time.Millisecond, ProjectLimit: 200, BackfillLimit: 100})
	}()
	select {
	case <-slow.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the backfill never started")
	}
	// the backfill is now stuck in the embedder; a new event must still be projected
	fresh := e.must(201, "POST", "/v1/memory/records", "bob", memory("assistant", "fresh", "tenant", "", "fresh row written while the backfill is stuck", "internal", e.now))["recordId"].(string)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var open int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM projection_events WHERE object_id = $1 AND processed_at IS NULL`, fresh).Scan(&open); err != nil {
			t.Fatal(err)
		}
		if open == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the event waited behind the slow backfill")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// both loops stop on shutdown, also with the backfill still blocked
	stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the workers did not stop")
	}
	releaseOnce.Do(func() { close(slow.release) })
}

// hookEmbedder runs a function while an embedding call is in flight.
type hookEmbedder struct {
	*blockingEmbedder
	during func()
}

func (h *hookEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	h.during()
	return h.inner.Embed(ctx, texts)
}

// The backfill runs beside the projection (D79): a vector it computed for a row that moved on, or was erased, meanwhile is not written.
func TestBackfillWritesNoVectorForARowThatChangedMeanwhile(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"a newer version", `UPDATE memory_records SET version = version + 1 WHERE content LIKE 'backfillguard%'`},
		{"an erasure", `UPDATE memory_records SET erasure_id = (SELECT id FROM erasures LIMIT 1) WHERE content LIKE 'backfillguard%'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			retireSetup(e)
			ctx := context.Background()
			conn := retireOwner(e, t)
			e.store.Embedder = nil
			id := e.must(201, "POST", "/v1/memory/records", "bob", memory("assistant", "guard", "tenant", "", "backfillguard row", "internal", e.now))["recordId"].(string)
			doc := e.must(201, "POST", "/api/v1/knowledge/documents", "alice", kbDoc("handbook", "handbook://g", "", "guard document"))["documentId"].(string)
			if tc.name == "an erasure" {
				e.must(201, "POST", "/api/v1/erasures", "alice", map[string]any{"documentIds": []string{doc}, "reason": "x"})
			}
			e.store.Embedder = &hookEmbedder{blockingEmbedder: &blockingEmbedder{inner: e.embedder}, during: func() {
				if _, err := conn.Exec(ctx, tc.sql); err != nil {
					t.Error(err)
				}
			}}
			if _, err := e.store.BackfillEmbeddings(ctx, 100); err != nil {
				t.Fatal(err)
			}
			var withVector bool
			if err := conn.QueryRow(ctx, `SELECT embedding IS NOT NULL FROM memory_records WHERE id = $1`, id).Scan(&withVector); err != nil || withVector {
				t.Fatalf("the row has a vector computed for a state it no longer has: %v %v", withVector, err)
			}
		})
	}
}
