// Package convert is the background worker that turns the originals of knowledge documents (PDF, Word, PowerPoint, Excel) into
// text (ADR-0001 D61 to D70). It takes jobs from the queue in the database and, for each, in a dedicated idp-sandbox instance
// reached through the platform's system gateway:
//
//  1. creates a workspace,
//  2. imports the original with a read ticket (the sandbox fetches it from CMI's internal file endpoint),
//  3. runs the converter in the office tier,
//  4. exports the Markdown with a write ticket (the sandbox posts it to the endpoint, which stages it),
//  5. deletes the workspace, whatever happened.
//
// CMI then reads the staged text, takes the converter's description markers out (cmi.CleanExtracted), chunks it
// (cmi.ChunkKnowledge, which reads the page, slide and sheet markers) and commits it in one transaction: while a job runs the
// version keeps the text it had, so half a document is never searchable. The embedding follows through the projection.
//
// The tenant of the document is never the sandbox's concern: every job runs as the one ingest workload, with a workspace of
// its own that is deleted at the end, and a ticket for this one document in this one direction.
package convert

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/cmi"
	"idp-cmi/internal/mcpclient"
	"idp-cmi/internal/ticket"
)

// The converters' contract is docs/adr/kb3-converter-format.md of the platform repository.
const (
	// ResultPath is where the converter writes its Markdown in the workspace.
	ResultPath = "out/extract.md"
	// MaxRows and MaxCols are the limits of a worksheet (decided 2026-11-25): beyond them the converter writes the first rows and
	// the columns up to the limit, and says so in a truncated marker.
	MaxRows = 10000
	MaxCols = 200
)

// Converter exit codes (kb3-converter-format.md section 2).
const (
	exitInternal    = 1
	exitUsage       = 2
	exitUnsupported = 3
	exitCorrupt     = 4
	exitLimits      = 5
	exitEmpty       = 6
)

// Error codes of a conversion that did not succeed (the "error" of the document's ingest state).
const (
	CodeSandboxBusy      = "sandbox_busy"
	CodeGatewayDown      = "gateway_unavailable"
	CodeGatewayTimeout   = "gateway_timeout"
	CodeStoreDown        = "store_unavailable"
	CodeSandboxError     = "sandbox_error"
	CodeConverterIntern  = "converter_internal"
	CodeCommitFailed     = "commit_failed"
	CodeImportRefused    = "import_refused"
	CodeExportRefused    = "export_refused"
	CodeUnsupported      = "unsupported"
	CodeCorrupt          = "corrupt"
	CodeLimits           = "limits"
	CodeNoText           = "no_text"
	CodeConversionFailed = "conversion_failed"
	CodeTimeout          = "conversion_timeout"
	CodeOOM              = "conversion_oom"
	CodeNoResult         = "no_result"
	CodeEmptyOutput      = "empty_output"
	CodeTextTooLarge     = "text_too_large"
	CodeOutputInvalid    = "output_invalid"
	CodeQuota            = "quota_exceeded"
	CodeDocumentChanged  = "document_changed"
	CodeBadConverter     = "converter_usage"
)

// Store is what the worker needs of the database.
type Store interface {
	ClaimConversions(ctx context.Context, owner string, limit int) ([]postgres.Conversion, error)
	ConversionWanted(ctx context.Context, c postgres.Conversion) (bool, error)
	SetConversionWorkspace(ctx context.Context, c postgres.Conversion, ws string) error
	RetryConversionLater(ctx context.Context, c postgres.Conversion, e postgres.ConversionError, delay time.Duration, limit int) error
	FailConversion(ctx context.Context, c postgres.Conversion, e postgres.ConversionError) error
	ReleaseConversion(ctx context.Context, c postgres.Conversion) error
	StagedConversion(ctx context.Context, c postgres.Conversion, limit int64) ([]byte, bool, error)
	ConversionResultKey(ctx context.Context, c postgres.Conversion) string
	DeleteStaged(ctx context.Context, key string)
	CommitConversion(ctx context.Context, c postgres.Conversion, text string, info cmi.ExtractInfo) (*postgres.Committed, error)
	PurgeTicketUses(ctx context.Context) (int, error)
}

// Gateway calls tools of the ingest sandbox.
type Gateway interface {
	CallTool(ctx context.Context, name string, args map[string]any) (*mcpclient.Result, error)
}

// Worker runs the conversions.
type Worker struct {
	Store   Store
	Gateway Gateway
	Signer  *ticket.Signer
	// Workload is the identity the sandbox sees as its caller; the tickets carry it as tenant_id and sub.
	WorkloadTenant, WorkloadSubject string
	// ToolPrefix is what the gateway puts before the ingest sandbox's tool names (the target's name and a separator: "ingest__").
	ToolPrefix string
	Owner      string // this replica's name (the lease owner)
	// Concurrency is how many conversions run at once; it must not exceed the ingest sandbox's own limit (IDP_SANDBOX_MAX_CONCURRENT),
	// which refuses the next run outright.
	Concurrency int
	Interval    time.Duration // how often the queue is looked at (default 5 s)
	// RunSeconds is the time limit of the converter's run in the sandbox (default 300; the instance's own limit is the ceiling).
	RunSeconds int
	// JobTimeout bounds one attempt (default 15 minutes, which must stay under the lease).
	JobTimeout time.Duration
	MaxResult  int64 // the largest converted text accepted (default 6 MiB)
	// Backoff is the wait after a transient failure of attempt n (default postgres.Backoff: 30 s, doubling, at most 30 minutes).
	Backoff func(attempt int) time.Duration
	Log     *slog.Logger
	// Finished is told how every attempt ended: "succeeded", "truncated", "retry" or "failed", and the error code.
	Finished func(outcome, code string)
	Now      func() time.Time

	mu       sync.Mutex
	inflight int
}

// attemptError is how an attempt failed.
type attemptError struct {
	code      string
	detail    string
	transient bool
	busy      bool // the sandbox (or CMI's own endpoint) was busy: the attempt is not spent
	limit     int  // a cap on the attempts of this kind of failure (0 = the queue's)
	cause     error
}

func (e *attemptError) Error() string { return e.code + ": " + e.detail }

// Unwrap keeps the cause, so that a cancelled context is still seen as one.
func (e *attemptError) Unwrap() error { return e.cause }

func busy(code, detail string) *attemptError {
	return &attemptError{code: code, detail: detail, transient: true, busy: true}
}

func transient(code, detail string) *attemptError {
	return &attemptError{code: code, detail: detail, transient: true}
}
func permanent(code, detail string) *attemptError { return &attemptError{code: code, detail: detail} }

func (w *Worker) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

func (w *Worker) interval() time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return 5 * time.Second
}

// Run takes jobs until ctx ends. Attempts still running then are given until the context of the shutdown; a job whose attempt
// is cut off keeps its lease and is taken again when it lapses.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.interval())
	defer t.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	purged := time.Time{}
	for {
		w.Tick(ctx, &wg)
		if now := time.Now(); now.Sub(purged) > 10*time.Minute {
			purged = now
			if _, err := w.Store.PurgeTicketUses(ctx); err != nil && ctx.Err() == nil {
				w.log().Warn("expired ticket records could not be purged", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick claims as many jobs as there are free places and starts them; wg tracks them.
func (w *Worker) Tick(ctx context.Context, wg *sync.WaitGroup) {
	if ctx.Err() != nil {
		return
	}
	limit := w.Concurrency
	if limit <= 0 {
		limit = 1
	}
	w.mu.Lock()
	free := limit - w.inflight
	w.mu.Unlock()
	if free <= 0 {
		return
	}
	jobs, err := w.Store.ClaimConversions(ctx, w.Owner, free)
	if err != nil {
		if ctx.Err() == nil {
			w.log().Warn("conversion jobs could not be claimed", "error", err)
		}
		return
	}
	for _, c := range jobs {
		w.mu.Lock()
		w.inflight++
		w.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				w.mu.Lock()
				w.inflight--
				w.mu.Unlock()
			}()
			w.Process(ctx, c)
		}()
	}
}

// Process makes one attempt at a job and records how it ended.
func (w *Worker) Process(ctx context.Context, c postgres.Conversion) {
	timeout := w.JobTimeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	log := w.log().With("job", c.ID, "document", c.DocumentID, "attempt", c.Attempts)
	if c.StaleResult != "" {
		w.Store.DeleteStaged(ctx, c.StaleResult)
	}
	if c.WorkspaceID != "" { // an attempt that died left its workspace; its idle timeout would collect it, this is quicker
		w.deleteWorkspace(c.WorkspaceID)
	}
	committed, err := w.attempt(actx, c)
	switch {
	case err == nil:
		log.Info("converted an original", "state", committed.State, "chunks", committed.Chunks)
		w.done(committed.State, "")
		return
	case errors.Is(err, postgres.ErrConversionLost):
		// the job is not ours any more (an erasure, a newer original, another worker)
		log.Info("a conversion attempt was dropped", "reason", errString(err))
		return
	case ctx.Err() != nil:
		// CMI is shutting down: the attempt was cut off, not failed, so it is given back unspent
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer rcancel()
		if rerr := w.Store.ReleaseConversion(rctx, c); rerr != nil {
			log.Warn("an interrupted conversion could not be released; its lease will lapse", "error", rerr)
		}
		return
	}
	var ae *attemptError
	if !errors.As(err, &ae) {
		ae = transient(CodeCommitFailed, err.Error())
	}
	if actx.Err() != nil && ctx.Err() == nil && !ae.transient { // the attempt's own time ran out
		ae = transient(CodeGatewayTimeout, "the attempt took longer than "+timeout.String())
	}
	ae.detail = postgres.RedactTickets(ae.detail)
	e := postgres.ConversionError{Code: ae.code, Detail: ae.detail, Busy: ae.busy}
	// the bookkeeping must survive a shutdown that cuts the attempt off
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer bcancel()
	if ae.transient {
		delay := postgres.Backoff(c.Attempts)
		if ae.busy { // the attempt is given back, so the wait does not grow with it: a minute and a bit, spread
			delay = time.Minute + time.Duration(rand.Int64N(int64(30*time.Second)))
		}
		if w.Backoff != nil {
			delay = w.Backoff(c.Attempts)
		}
		err = w.Store.RetryConversionLater(bctx, c, e, delay, ae.limit)
		log.Warn("a conversion attempt failed and will be tried again", "error", ae.code, "detail", ae.detail, "in", delay.String())
		w.done("retry", ae.code)
	} else {
		err = w.Store.FailConversion(bctx, c, e)
		log.Warn("a conversion failed", "error", ae.code, "detail", ae.detail)
		w.done("failed", ae.code)
	}
	if err != nil && !errors.Is(err, postgres.ErrConversionLost) {
		log.Error("the outcome of a conversion could not be recorded", "error", err)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (w *Worker) done(outcome, code string) {
	if w.Finished != nil {
		w.Finished(outcome, code)
	}
}

func (w *Worker) tool(name string) string { return w.ToolPrefix + name }

// attempt is the pipeline of one attempt.
func (w *Worker) attempt(ctx context.Context, c postgres.Conversion) (*postgres.Committed, error) {
	ext, ok := postgres.IngestExtension(c.MediaType)
	if !ok {
		return nil, permanent(CodeUnsupported, "the media type "+c.MediaType+" is not converted")
	}
	if wanted, err := w.Store.ConversionWanted(ctx, c); err != nil {
		return nil, transient(CodeStoreDown, err.Error())
	} else if !wanted { // withdrawn, erased or given a newer original while it waited: nothing for the sandbox to do
		return nil, permanent(CodeDocumentChanged, "the document no longer has the original this conversion was queued for")
	}
	ws, err := w.create(ctx)
	if err != nil {
		return nil, err
	}
	if err := w.Store.SetConversionWorkspace(ctx, c, ws); err != nil {
		w.deleteWorkspace(ws)
		return nil, err
	}
	defer func() { // whatever happened, the workspace goes; the original and the text it held were this one document's
		w.deleteWorkspace(ws)
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = w.Store.SetConversionWorkspace(bctx, c, "")
	}()
	in := "in/original." + ext
	if err := w.importFile(ctx, c, ws, in); err != nil {
		return nil, err
	}
	if err := w.run(ctx, ws, ext, in); err != nil {
		return nil, err
	}
	if err := w.exportFile(ctx, c, ws); err != nil {
		return nil, err
	}
	return w.commit(ctx, c)
}

func (w *Worker) create(ctx context.Context) (string, error) {
	res, err := w.Gateway.CallTool(ctx, w.tool("sandbox_create"), nil)
	if err != nil {
		return "", classifyCall(err)
	}
	if res.IsError {
		return "", classifyTool(res.Text, "sandbox_create")
	}
	var out struct {
		WorkspaceID string `json:"workspaceId"`
	}
	if err := res.Decode(&out); err != nil || out.WorkspaceID == "" {
		return "", transient(CodeSandboxError, "sandbox_create returned no workspace id")
	}
	return out.WorkspaceID, nil
}

// deleteWorkspace removes a workspace; it is best effort (the instance's idle timeout collects what this cannot reach).
func (w *Worker) deleteWorkspace(ws string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := w.Gateway.CallTool(ctx, w.tool("sandbox_delete"), map[string]any{"workspaceId": ws})
	if err != nil || res.IsError {
		w.log().Warn("an ingest workspace could not be deleted; the instance's idle timeout will collect it", "workspace", ws, "error", errString(err))
	}
}

// ticketFor signs a ticket for this attempt of the job in one direction.
func (w *Worker) ticketFor(c postgres.Conversion, kind string) (string, error) {
	tok, _, err := w.Signer.Sign(ticket.Claims{Tenant: w.WorkloadTenant, Subject: w.WorkloadSubject, Kind: kind,
		DocTenant: c.Tenant, Document: c.DocumentID, Version: c.Version, Job: c.ID, Attempt: c.Attempts, Key: c.OriginalKey, SHA256: c.OriginalSHA256})
	return tok, err
}

func (w *Worker) importFile(ctx context.Context, c postgres.Conversion, ws, path string) error {
	tok, err := w.ticketFor(c, ticket.Read)
	if err != nil {
		return permanent(CodeBadConverter, err.Error())
	}
	res, err := w.Gateway.CallTool(ctx, w.tool("sandbox_import_file"), map[string]any{"workspaceId": ws, "source": tok, "path": path})
	if err != nil {
		return classifyCall(err)
	}
	if res.IsError {
		return classifyTransfer(res.Text, CodeImportRefused)
	}
	return nil
}

// runCode is the script the office tier runs: the converter the original's type calls for, writing ResultPath.
func runCode(ext, in string) string {
	switch ext {
	case "pdf":
		return fmt.Sprintf("mkdir -p out && pdftext %s --markdown %s\n", in, ResultPath)
	case "xlsx":
		return fmt.Sprintf("mkdir -p out && ooxml2md %s -o %s --max-rows %d --max-cols %d\n", in, ResultPath, MaxRows, MaxCols)
	}
	return fmt.Sprintf("mkdir -p out && ooxml2md %s -o %s\n", in, ResultPath)
}

func (w *Worker) run(ctx context.Context, ws, ext, in string) error {
	secs := w.RunSeconds
	if secs <= 0 {
		secs = 300
	}
	res, err := w.Gateway.CallTool(ctx, w.tool("sandbox_run"), map[string]any{"workspaceId": ws, "language": "office", "code": runCode(ext, in), "timeoutSeconds": secs, "network": "none"})
	if err != nil {
		return classifyCall(err)
	}
	if res.IsError {
		return classifyTool(res.Text, "sandbox_run")
	}
	var out struct {
		ExitCode    int    `json:"exitCode"`
		Stderr      string `json:"stderr"`
		TimedOut    bool   `json:"timedOut"`
		OutOfMemory bool   `json:"outOfMemory"`
	}
	if err := res.Decode(&out); err != nil {
		return transient(CodeSandboxError, "sandbox_run returned no result")
	}
	detail := firstLine(out.Stderr)
	switch {
	case out.TimedOut:
		return permanent(CodeTimeout, "the converter did not finish in "+fmt.Sprint(secs)+" seconds")
	case out.OutOfMemory, strings.Contains(out.Stderr, "OutOfMemoryException"): // officecli exits 0 when it runs out of memory
		return permanent(CodeOOM, detail)
	}
	switch out.ExitCode {
	case 0:
		return nil
	case exitInternal:
		e := transient(CodeConverterIntern, detail)
		e.limit = 2 // tried once more, as the format says
		return e
	case exitUsage:
		return permanent(CodeBadConverter, detail)
	case exitUnsupported:
		return permanent(CodeUnsupported, detail)
	case exitCorrupt:
		return permanent(CodeCorrupt, detail)
	case exitLimits:
		return permanent(CodeLimits, detail)
	case exitEmpty:
		return permanent(CodeNoText, detail)
	}
	return permanent(CodeConversionFailed, fmt.Sprintf("exit code %d: %s", out.ExitCode, detail))
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return cmi.CutUTF8(s, 300)
}

func (w *Worker) exportFile(ctx context.Context, c postgres.Conversion, ws string) error {
	tok, err := w.ticketFor(c, ticket.Write)
	if err != nil {
		return permanent(CodeBadConverter, err.Error())
	}
	res, err := w.Gateway.CallTool(ctx, w.tool("sandbox_export_file"), map[string]any{"workspaceId": ws, "path": ResultPath, "target": tok, "name": "extract.md"})
	if err != nil {
		return classifyCall(err)
	}
	if res.IsError {
		e := classifyTransfer(res.Text, CodeExportRefused)
		if !e.transient && strings.Contains(res.Text, "transfer limit") {
			return permanent(CodeTextTooLarge, "the converted text is larger than the limit")
		}
		return e
	}
	return nil
}

// commit reads what the endpoint staged and writes it as the text of the version.
func (w *Worker) commit(ctx context.Context, c postgres.Conversion) (*postgres.Committed, error) {
	limit := w.MaxResult
	if limit <= 0 {
		limit = 6 << 20
	}
	key := w.Store.ConversionResultKey(ctx, c)
	data, ok, err := w.Store.StagedConversion(ctx, c, limit)
	if err != nil {
		if errors.Is(err, postgres.ErrConversionLost) {
			return nil, err
		}
		return nil, transient(CodeStoreDown, err.Error())
	}
	defer w.Store.DeleteStaged(ctx, key)
	if !ok {
		return nil, permanent(CodeNoResult, "the sandbox delivered no result")
	}
	if !utf8.Valid(data) {
		return nil, permanent(CodeOutputInvalid, "the converted text is not UTF-8")
	}
	text, info := cmi.CleanExtracted(string(data))
	if !info.HasBody { // an empty output is a failure, whatever the converter's exit code said
		return nil, permanent(CodeEmptyOutput, "the converted text has no content")
	}
	if len(text) > cmi.MaxDocument {
		return nil, permanent(CodeTextTooLarge, fmt.Sprintf("the converted text is %d bytes; at most %d", len(text), cmi.MaxDocument))
	}
	committed, err := w.Store.CommitConversion(ctx, c, text, info)
	switch {
	case err == nil:
		return committed, nil
	case errors.Is(err, postgres.ErrConversionLost):
		return nil, err
	case errors.Is(err, postgres.ErrNotCommittable):
		return nil, permanent(CodeDocumentChanged, "the document no longer has the original this conversion was made from")
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && strings.HasPrefix(pg.Code, "22") { // data exception: the database cannot hold this text, and will not the next time
		return nil, permanent(CodeOutputInvalid, "the database refused the text ("+pg.Code+")")
	}
	var se *postgres.Error
	if errors.As(err, &se) && se.Code == "QUOTA_EXCEEDED" {
		return nil, permanent(CodeQuota, se.Message)
	}
	if errors.As(err, &se) && se.Code == "INVALID" {
		return nil, permanent(CodeOutputInvalid, se.Message)
	}
	return nil, transient(CodeCommitFailed, err.Error())
}

// ---- classification of what the gateway and the sandbox answer -------------------------------------------------------------

// classifyCall is a call that got no tool result: the gateway did not answer, answered with a status, or refused.
func classifyCall(err error) *attemptError {
	e := classifyCallKind(err)
	e.cause = err
	return e
}

func classifyCallKind(err error) *attemptError {
	var ne *mcpclient.NetworkError
	var he *mcpclient.HTTPError
	var re *mcpclient.RPCError
	switch {
	case errors.As(err, &ne) && ne.Timeout:
		// the gateway ends a call after its own limit (60 s unless the operator raised it) and then says nothing of the outcome
		return transient(CodeGatewayTimeout, "the gateway call timed out")
	case errors.As(err, &ne):
		return transient(CodeGatewayDown, ne.Error())
	case mcpclient.Unauthorized(err):
		return transient(CodeGatewayDown, "the gateway refused CMI's credential")
	case errors.As(err, &he):
		return transient(CodeGatewayDown, he.Error())
	case errors.As(err, &re):
		return transient(CodeGatewayDown, re.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return transient(CodeGatewayTimeout, "the attempt ran out of time")
	}
	return transient(CodeGatewayDown, err.Error())
}

// classifyTool is a tool that answered isError: the sandbox's own words (idp-sandbox's failure messages).
func classifyTool(text, tool string) *attemptError {
	switch {
	case strings.Contains(text, "at capacity"), strings.Contains(text, "maximum number of workspaces"), strings.Contains(text, "running another command"):
		return busy(CodeSandboxBusy, cmi.CutUTF8(text, 200)) // the 9th concurrent run is refused outright, not queued
	case strings.Contains(text, "failed to complete the request"):
		return transient(CodeSandboxError, tool+": "+cmi.CutUTF8(text, 200))
	}
	return permanent(CodeSandboxError, tool+": "+cmi.CutUTF8(text, 200))
}

// classifyTransfer is an import or an export that was refused. A store that is away, or a file endpoint that answered 5xx, is
// transient; a ticket, a file the sandbox does not accept (a type, a macro, a bomb) or one over the transfer limit is not.
func classifyTransfer(text, refused string) *attemptError {
	switch {
	case strings.Contains(text, "(HTTP 429)"), strings.Contains(text, "at capacity"), strings.Contains(text, "running another command"):
		return busy(CodeSandboxBusy, cmi.CutUTF8(text, 200)) // CMI's own endpoint is at its limit of reads, or the sandbox is busy
	case strings.Contains(text, "did not answer"), strings.Contains(text, "(HTTP 5"):
		return transient(CodeStoreDown, cmi.CutUTF8(text, 200))
	}
	return permanent(refused, cmi.CutUTF8(text, 200))
}
