package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
	"idp-cmi/internal/guard"
	"idp-cmi/internal/ticket"
)

// The conversion of an original into text (ADR-0001 D61 to D70): a queue of jobs, the staging of a result, and the commit that
// replaces the text of a version in one transaction.

// Conversion states.
const (
	ConvQueued    = "queued"
	ConvRunning   = "running"
	ConvSucceeded = "succeeded"
	ConvTruncated = "truncated"
	ConvFailed    = "failed"
)

// ConversionSettings are the operator's settings for the conversion queue.
type ConversionSettings struct {
	// Enabled makes the storing of a PDF or Office original queue a conversion. Off, originals are only stored (D50).
	Enabled bool
	// MaxAttempts is how many times a job is tried before it fails (default 6); a transient failure waits Backoff first.
	MaxAttempts int
	// Lease is how long a worker holds a job before another may take it (default 20 minutes).
	Lease time.Duration
	// SandboxConcurrency is the ingest sandbox's own limit on runs (0 = not enforced here). It holds over all replicas: a claim
	// takes no more jobs than the limit leaves after the running jobs whose lease is live.
	SandboxConcurrency int
	// MaxBusyWait is how long a job that keeps meeting a busy sandbox may wait in all before that counts as a failure (default 6 hours).
	MaxBusyWait time.Duration
}

// DefaultConversionSettings are what Open starts with.
func DefaultConversionSettings() ConversionSettings {
	return ConversionSettings{MaxAttempts: 6, Lease: 20 * time.Minute}
}

func (c ConversionSettings) maxAttempts() int {
	if c.MaxAttempts <= 0 {
		return 6
	}
	return c.MaxAttempts
}

func (c ConversionSettings) maxBusyWait() time.Duration {
	if c.MaxBusyWait <= 0 {
		return 6 * time.Hour
	}
	return c.MaxBusyWait
}

func (c ConversionSettings) lease() time.Duration {
	if c.Lease <= 0 {
		return 20 * time.Minute
	}
	return c.Lease
}

// Backoff is the wait before attempt n+1 after a transient failure of attempt n: 30 s, doubling, at most 30 minutes.
func Backoff(attempt int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < attempt && d < 30*time.Minute; i++ {
		d *= 2
	}
	return min(d, 30*time.Minute)
}

// ingestibleMedia are the originals whose text is extracted (PDF and the three Office formats). Images wait for OCR (KB4); text
// files are their own text.
var ingestibleMedia = map[string]string{
	"application/pdf": "pdf",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   "docx",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": "pptx",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         "xlsx",
}

// IngestExtension is the extension a converter is given for the media type, and whether the type is converted at all.
func IngestExtension(media string) (string, bool) {
	e, ok := ingestibleMedia[media]
	return e, ok
}

// Conversion is a queued or running job as the worker sees it.
type Conversion struct {
	ID             string
	Tenant         string // the database's spelling
	DocumentID     string
	Version        int
	OriginalKey    string
	OriginalSHA256 string
	MediaType      string
	ByteSize       int64
	RequestedBy    string
	Attempts       int
	WorkspaceID    string // of an attempt that did not clean up, so that it can be
	StaleResult    string // the staged result of an earlier attempt, to be deleted
}

// conversionCols are the columns of Conversion, in the order scanConversion reads them.
const conversionCols = `j.id::text, j.tenant_id::text, j.document_id::text, j.document_version, j.original_key, j.original_sha256, j.media_type, j.byte_size, j.requested_by, j.attempts, coalesce(j.workspace_id, '')`

// checkSourceTextUnchanged is the rule of D77: a version that is or was in force never changes its source text. Only a version
// that waits for its first text (pending: it was written with awaitsOriginal) takes a convertible original; a version that
// holds extracted text takes the conversion of the same original again (a retry, a converter that was upgraded), the one
// in-place correction there is; every other version - active with the text its writer supplied, superseded, revoked, expired -
// is refused: the writer writes a new version with the original. sameOriginal says the original is the one the document has.
func checkSourceTextUnchanged(docID, status, source string, sameOriginal bool) error {
	switch {
	case status == "pending":
		return nil
	case (status == "active" || status == "superseded") && source == "extracted" && sameOriginal:
		return nil
	}
	return fail("CONFLICT", "version %s is %s and has been in force: its text does not change. Write a new version (awaitsOriginal) and store the original with it", docID, status)
}

// enqueueConversion queues the conversion of the original a document now has (the caller holds the document row). A version
// that waits for its first text is not in force until it is extracted (D64, D77); one that holds extracted text is corrected in
// place. Earlier jobs of the document that are still open are given up.
func enqueueConversion(ctx context.Context, tx pgx.Tx, tenant, docID, by, media, key, sum string, size int64) (stale []string, err error) {
	var status, source string
	if err := tx.QueryRow(ctx, `SELECT status, text_source FROM knowledge_documents WHERE id = $1`, docID).Scan(&status, &source); err != nil {
		return nil, err
	}
	if err := checkSourceTextUnchanged(docID, status, source, true); err != nil {
		return nil, err
	}
	// the staged text of a job that is given up is plaintext under the document's prefix: its object goes after the commit
	rows, err := tx.Query(ctx, `WITH old AS (SELECT id, result_key FROM knowledge_conversions WHERE document_id = $1 AND state IN ('queued', 'running') FOR UPDATE)
		UPDATE knowledge_conversions j SET state = 'failed', error_code = 'replaced', error_detail = 'a newer original was stored', finished_at = now(), updated_at = now(), result_key = NULL
		FROM old WHERE j.id = old.id RETURNING coalesce(old.result_key, '')`, docID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return nil, err
		}
		if k != "" {
			stale = append(stale, k)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	id := NewUUID()
	if _, err := tx.Exec(ctx, `INSERT INTO knowledge_conversions (id, tenant_id, document_id, document_version, original_key, original_sha256, media_type, byte_size, requested_by)
		SELECT $1, d.tenant_id, d.id, d.version, $3, $4, $5, $6, $7 FROM knowledge_documents d WHERE d.id = $2`, id, docID, key, sum, media, size, by); err != nil {
		return nil, err
	}
	return stale, Audit(ctx, tx, tenant, by, "knowledge.ingest.queued", docID, map[string]any{"job": id, "mediaType": media, "bytes": size})
}

// RetryConversion queues a new conversion of the document's original (a failed one, or another go at a finished one). The
// caller must be able to write the document's base, as for storing the original.
func (s *Store) RetryConversion(ctx context.Context, tenant, id string, v Visibility) (*cmi.Document, error) {
	if !s.Conversion.Enabled {
		return nil, fail("NOT_CONFIGURED", "originals are not converted on this service")
	}
	var out *cmi.Document
	var stale []string
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := bindView(ctx, tx, v); err != nil {
			return err
		}
		t, err := findForOriginal(ctx, tx, tenant, v, id, true)
		if err != nil {
			return err
		}
		if _, ok := IngestExtension(t.media); !ok || t.key == "" {
			return fail("INVALID", "the document has no original that is converted to text")
		}
		var status string
		var open int
		if err := tx.QueryRow(ctx, `SELECT d.status, (SELECT count(*) FROM knowledge_conversions j WHERE j.document_id = d.id AND j.state IN ('queued', 'running'))
			FROM knowledge_documents d WHERE d.id = $1 FOR UPDATE`, t.id).Scan(&status, &open); err != nil {
			return err
		}
		if status != "active" && status != "superseded" && status != "pending" {
			return fail("CONFLICT", "document %s is %s", t.id, status)
		}
		if open > 0 {
			return fail("CONFLICT", "document %s is being converted already", t.id)
		}
		if stale, err = enqueueConversion(ctx, tx, tenant, t.id, v.Subject, t.media, t.key, t.sum, t.size); err != nil {
			return err
		}
		out, err = scanDoc(tx.QueryRow(ctx, `SELECT `+docCols+` FROM knowledge_documents d JOIN sources s ON s.id = d.source_id WHERE d.id = $1`, t.id))
		return err
	})
	if err == nil {
		for _, k := range stale {
			s.DeleteStaged(ctx, k)
		}
	}
	return out, err
}

// ClaimConversions takes up to limit jobs that are due: queued and past their retry time, or running with a lapsed lease
// (their worker died). Jobs that have used all their attempts are failed instead. It works across tenants, as the embedder
// does, under its own role.
func (s *Store) ClaimConversions(ctx context.Context, owner string, limit int) ([]Conversion, error) {
	var out []Conversion
	var orphans []string
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.role', 'converter', true)`); err != nil {
			return err
		}
		// one claim at a time over all replicas, so that the limit below is a limit
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(72095)`); err != nil {
			return err
		}
		dead, err := tx.Query(ctx, `WITH old AS (SELECT id, result_key FROM knowledge_conversions WHERE state = 'running' AND lease_until < now() AND attempts >= $1 FOR UPDATE)
			UPDATE knowledge_conversions j SET state = 'failed', error_code = 'attempts_exhausted', error_detail = 'the worker died on the last attempt', finished_at = now(), updated_at = now(), lease_owner = NULL, result_key = NULL
			FROM old WHERE j.id = old.id RETURNING coalesce(old.result_key, '')`, s.Conversion.maxAttempts())
		if err != nil {
			return err
		}
		for dead.Next() {
			var k string
			if err := dead.Scan(&k); err != nil {
				dead.Close()
				return err
			}
			if k != "" {
				orphans = append(orphans, k)
			}
		}
		dead.Close()
		if err := dead.Err(); err != nil {
			return err
		}
		if cap := s.Conversion.SandboxConcurrency; cap > 0 {
			var live int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM knowledge_conversions WHERE state = 'running' AND lease_until >= now()`).Scan(&live); err != nil {
				return err
			}
			if limit = min(limit, cap-live); limit <= 0 {
				return nil
			}
		}
		rows, err := tx.Query(ctx, `WITH due AS (
				SELECT id, result_key AS stale FROM knowledge_conversions
				WHERE (state = 'queued' AND next_attempt_at <= now()) OR (state = 'running' AND lease_until < now() AND attempts < $4)
				ORDER BY next_attempt_at, created_at LIMIT $3 FOR UPDATE SKIP LOCKED)
			UPDATE knowledge_conversions j SET state = 'running', lease_owner = $1, lease_until = now() + $2::interval, attempts = j.attempts + 1,
				started_at = coalesce(j.started_at, now()), updated_at = now(), result_key = NULL, result_sha256 = NULL, result_bytes = NULL
			FROM due WHERE j.id = due.id
			RETURNING `+conversionCols+`, coalesce(due.stale, '')`, owner, s.Conversion.lease().String(), limit, s.Conversion.maxAttempts())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c Conversion
			if err := rows.Scan(&c.ID, &c.Tenant, &c.DocumentID, &c.Version, &c.OriginalKey, &c.OriginalSHA256, &c.MediaType, &c.ByteSize, &c.RequestedBy, &c.Attempts, &c.WorkspaceID, &c.StaleResult); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err == nil {
		for _, k := range orphans { // the staged text of a job that has no attempts left
			s.DeleteStaged(ctx, k)
		}
	}
	return out, err
}

// ConversionWanted reports whether the document still has the original the job is for and may still be given text: it is not
// erased, withdrawn or expired. A job that is not wanted any more is ended before the sandbox is asked to do anything.
func (s *Store) ConversionWanted(ctx context.Context, c Conversion) (bool, error) {
	wanted := false
	err := s.InTenant(ctx, c.Tenant, func(tx pgx.Tx) error {
		var key, sum string
		var erased bool
		var status string
		err := tx.QueryRow(ctx, `SELECT d.status, d.erasure_id IS NOT NULL, coalesce(d.original_key, ''), coalesce(d.original_sha256, '')
			FROM knowledge_documents d WHERE d.id = $1`, c.DocumentID).Scan(&status, &erased, &key, &sum)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		wanted = err == nil && !erased && key == c.OriginalKey && sum == c.OriginalSHA256 && (status == "pending" || status == "active" || status == "superseded")
		return err
	})
	return wanted, err
}

// SetConversionWorkspace records the sandbox workspace of the running attempt, and clears it (ws "") when the workspace is gone.
func (s *Store) SetConversionWorkspace(ctx context.Context, c Conversion, ws string) error {
	return s.InTenant(ctx, c.Tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE knowledge_conversions SET workspace_id = nullif($3, ''), updated_at = now() WHERE id = $1 AND state = 'running' AND attempts = $2`, c.ID, c.Attempts, ws)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrConversionLost
		}
		return nil
	})
}

// ErrConversionLost: the job is not this attempt's any more (another worker took it, it was given up, or the document was erased).
var ErrConversionLost = errors.New("the conversion job is no longer this attempt's")

// ConversionError is how an attempt failed.
type ConversionError struct {
	Code   string // a short stable word: sandbox_busy, conversion_failed, ...
	Detail string // for the log and the audit, never shown to the uploader
	Busy   bool   // the sandbox or CMI's own endpoint was busy: not a failure of the job, the attempt is not spent
}

// RetryConversionLater puts the job back in the queue after a transient failure, to run again after delay; when the attempt was
// the last it fails. A limit above 0 caps the attempts of this kind of failure (a converter's own error is tried twice). A busy
// sandbox is not a failure of the job: the attempt is given back, and only a job that has waited MaxBusyWait in all fails.
func (s *Store) RetryConversionLater(ctx context.Context, c Conversion, e ConversionError, delay time.Duration, limit int) error {
	e.Detail = redactTickets(e.Detail)
	last := s.Conversion.maxAttempts()
	if limit > 0 && limit < last {
		last = limit
	}
	if !e.Busy && c.Attempts >= last {
		return s.FailConversion(ctx, c, e)
	}
	give := 0
	if e.Busy {
		give = 1
	}
	failed := false
	err := s.InTenant(ctx, c.Tenant, func(tx pgx.Tx) error {
		if e.Busy {
			var waited bool
			if err := tx.QueryRow(ctx, `SELECT coalesce(now() - started_at > $2::interval, false) FROM knowledge_conversions WHERE id = $1`, c.ID, s.Conversion.maxBusyWait().String()).Scan(&waited); err != nil {
				return err
			}
			if waited {
				failed = true
				return nil
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE knowledge_conversions SET state = 'queued', next_attempt_at = now() + $3::interval, lease_owner = NULL, lease_until = NULL, attempts = greatest(attempts - $6, 0),
			error_code = $4, error_detail = $5, workspace_id = NULL, updated_at = now() WHERE id = $1 AND state = 'running' AND attempts = $2`,
			c.ID, c.Attempts, delay.String(), e.Code, cutDetail(e.Detail), give)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrConversionLost
		}
		return nil
	})
	if err == nil && failed {
		return s.FailConversion(ctx, c, e)
	}
	return err
}

// ReleaseConversion gives the attempt back without spending it (CMI is shutting down): the job is queued again at once.
func (s *Store) ReleaseConversion(ctx context.Context, c Conversion) error {
	return s.InTenant(ctx, c.Tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE knowledge_conversions SET state = 'queued', next_attempt_at = now(), lease_owner = NULL, lease_until = NULL, attempts = greatest(attempts - 1, 0),
			workspace_id = NULL, updated_at = now() WHERE id = $1 AND state = 'running' AND attempts = $2`, c.ID, c.Attempts)
		return err
	})
}

// FailConversion ends the job as failed. The version stays as it is: a first conversion that fails leaves it pending (not in
// force), so nothing half-made is searchable and the version it would have replaced stays current. The text the job had staged
// goes with it.
func (s *Store) FailConversion(ctx context.Context, c Conversion, e ConversionError) error {
	var staged string
	err := s.InTenant(ctx, c.Tenant, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT coalesce(result_key, '') FROM knowledge_conversions WHERE id = $1 AND state = 'running' AND attempts = $2 FOR UPDATE`, c.ID, c.Attempts).Scan(&staged)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConversionLost
		}
		if err != nil {
			return err
		}
		detail := cutDetail(redactTickets(e.Detail))
		if _, err := tx.Exec(ctx, `UPDATE knowledge_conversions SET state = 'failed', error_code = $3, error_detail = $4, lease_owner = NULL, lease_until = NULL,
			workspace_id = NULL, result_key = NULL, finished_at = now(), updated_at = now() WHERE id = $1 AND attempts = $2`, c.ID, c.Attempts, e.Code, detail); err != nil {
			return err
		}
		return Audit(ctx, tx, c.Tenant, c.RequestedBy, "knowledge.ingest.failed", c.DocumentID, map[string]any{"job": c.ID, "attempts": c.Attempts, "error": e.Code, "detail": detail})
	})
	if err == nil {
		s.DeleteStaged(ctx, staged)
	}
	return err
}

var ticketShaped = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`)

// RedactTickets is redactTickets for the worker, which logs the same text.
func RedactTickets(s string) string { return redactTickets(s) }

func redactTickets(s string) string { return ticketShaped.ReplaceAllString(s, "[redacted]") }

func cutDetail(s string) string { return cutUTF8(s, 300) }

// ---- tickets and the file endpoint -------------------------------------------------------------------------------------

// ErrTicketRefused is every refusal of the file endpoint: a ticket that was used, that names a job that is not running, a
// document that is gone or has another original. The endpoint answers all of them alike.
var ErrTicketRefused = errors.New("the ticket is not valid for this document")

// UseTicket records that the ticket was presented. It reports false when it had been presented before: a ticket works once.
// The record stays until the ticket would have expired anyway.
func (s *Store) UseTicket(ctx context.Context, c ticket.Claims) (bool, error) {
	if !IsUUID(c.Document) || !IsUUID(c.Job) || !IsUUID(c.DocTenant) {
		return false, ErrTicketRefused
	}
	first := false
	err := s.InTenant(ctx, c.DocTenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO knowledge_ticket_uses (jti, tenant_id, document_id, job_id, kind, expires_at)
			SELECT $1, d.tenant_id, d.id, $3, $4, to_timestamp($5) FROM knowledge_documents d WHERE d.id = $2
			ON CONFLICT (jti) DO NOTHING`, c.ID, c.Document, c.Job, c.Kind, c.Expires)
		first = err == nil && tag.RowsAffected() == 1
		return err
	})
	return first, err
}

// PurgeTicketUses forgets the tickets that have expired: a presented ticket only has to be remembered while it could still be
// presented.
func (s *Store) PurgeTicketUses(ctx context.Context) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.role', 'converter', true)`); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM knowledge_ticket_uses WHERE expires_at < now() - interval '1 hour'`)
		n = int(tag.RowsAffected())
		return err
	})
	return n, err
}

// ticketTarget is what a ticket is allowed to touch, as the database sees it now.
type ticketTarget struct {
	media string
	size  int64
	sum   string
}

// bindTicket checks that the ticket is for this attempt of a running job, and that the document still has the original the
// ticket names: not erased, not replaced, not revoked. Everything the ticket says is checked against the rows, because the
// sandbox, which carries the ticket, checks nothing of it.
func bindTicket(ctx context.Context, tx pgx.Tx, c ticket.Claims) (*ticketTarget, error) {
	resultFree := c.Kind == ticket.Write // one result for an attempt
	var t ticketTarget
	err := tx.QueryRow(ctx, `SELECT j.media_type, j.byte_size, j.original_sha256
		FROM knowledge_conversions j JOIN knowledge_documents d ON d.id = j.document_id
		WHERE j.id = $1 AND j.document_id = $2 AND j.tenant_id = current_tenant() AND j.state = 'running' AND j.lease_until > now() AND j.attempts = $3
		  AND j.original_key = $4 AND j.original_sha256 = $5 AND (NOT $7 OR j.result_key IS NULL)
		  AND d.original_key = $4 AND d.original_sha256 = $5 AND d.version = $6 AND d.erasure_id IS NULL AND d.status IN ('pending', 'active', 'superseded')`,
		c.Job, c.Document, c.Attempt, c.Key, c.SHA256, c.Version, resultFree).Scan(&t.media, &t.size, &t.sum)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTicketRefused
	}
	return &t, err
}

// ConversionOriginal opens the original a read ticket names, after checking the ticket against the database. The file is
// verified against its recorded digest before it is handed over.
func (s *Store) ConversionOriginal(ctx context.Context, c ticket.Claims) (*os.File, string, int64, error) {
	if s.Originals == nil {
		return nil, "", 0, fail("NOT_CONFIGURED", "no original store is configured")
	}
	var t *ticketTarget
	err := s.InTenant(ctx, c.DocTenant, func(tx pgx.Tx) (err error) {
		t, err = bindTicket(ctx, tx, c)
		return err
	})
	if err != nil {
		return nil, "", 0, err
	}
	f, err := s.openVerified(ctx, c.Document, c.Key, t.size, t.sum)
	return f, t.media, t.size, err
}

// StagedResult is a converted text the endpoint received.
type StagedResult struct {
	Ref    string
	Size   int64
	SHA256 string
}

// ErrResultTooLarge: the body is over the limit.
var ErrResultTooLarge = errors.New("the result is larger than the limit")

// StageConversionResult takes the converted text a write ticket delivers: the ticket is checked against the database, the body
// is read once under the limit and stored as an object under the document's prefix (so an erasure of the document sweeps it),
// and the job records it. Nothing is added to the document: that happens when the worker commits.
func (s *Store) StageConversionResult(ctx context.Context, c ticket.Claims, body io.Reader, limit int64) (*StagedResult, error) {
	if s.Originals == nil {
		return nil, fail("NOT_CONFIGURED", "no original store is configured")
	}
	err := s.InTenant(ctx, c.DocTenant, func(tx pgx.Tx) error {
		_, err := bindTicket(ctx, tx, c)
		return err
	})
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	var buf bytes.Buffer
	n, err := io.Copy(io.MultiWriter(&buf, h), io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fail("INVALID", "the body could not be read: %v", err)
	}
	if n > limit {
		return nil, ErrResultTooLarge
	}
	if n == 0 {
		return nil, fail("INVALID", "the result is empty")
	}
	sum := "sha256:" + hex.EncodeToString(h.Sum(nil))
	key := fmt.Sprintf("kb/%s/%s/%d/result-%s-%d-%s", c.DocTenant, c.Document, c.Version, c.Job, c.Attempt, c.ID) // the ticket's own, so that a refused second result deletes only itself
	if err := s.Originals.Put(ctx, key, buf.Bytes()); err != nil {
		return nil, unavailable(err)
	}
	err = s.InTenant(ctx, c.DocTenant, func(tx pgx.Tx) error {
		if _, err := bindTicket(ctx, tx, c); err != nil { // an erasure or a newer original that came while the body was read
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE knowledge_conversions SET result_key = $3, result_sha256 = $4, result_bytes = $5, updated_at = now()
			WHERE id = $1 AND attempts = $2 AND state = 'running' AND result_key IS NULL`, c.Job, c.Attempt, key, sum, n)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrTicketRefused
		}
		return nil
	})
	if err != nil {
		if derr := s.Originals.Delete(context.WithoutCancel(ctx), key); derr != nil && s.Log != nil {
			s.Log.Error("a staged conversion result that was not recorded could not be deleted", "key", key, "error", derr)
		}
		return nil, err
	}
	return &StagedResult{Ref: c.Job, Size: n, SHA256: sum}, nil
}

// StagedConversion reads the converted text the job's attempt received, checking its digest. ok is false when the sandbox
// reported success but no result arrived.
func (s *Store) StagedConversion(ctx context.Context, c Conversion, limit int64) (text []byte, ok bool, err error) {
	if s.Originals == nil {
		return nil, false, fail("NOT_CONFIGURED", "no original store is configured")
	}
	var key, sum string
	var size int64
	err = s.InTenant(ctx, c.Tenant, func(tx pgx.Tx) error {
		e := tx.QueryRow(ctx, `SELECT coalesce(result_key, ''), coalesce(result_sha256, ''), coalesce(result_bytes, 0) FROM knowledge_conversions WHERE id = $1 AND attempts = $2 AND state = 'running'`,
			c.ID, c.Attempts).Scan(&key, &sum, &size)
		if errors.Is(e, pgx.ErrNoRows) {
			return ErrConversionLost
		}
		return e
	})
	if err != nil || key == "" {
		return nil, false, err
	}
	data, err := s.Originals.Get(ctx, key, min(size, limit))
	if err != nil {
		return nil, false, unavailable(err)
	}
	h := sha256.Sum256(data)
	if int64(len(data)) != size || "sha256:"+hex.EncodeToString(h[:]) != sum {
		return nil, false, fmt.Errorf("the staged result of conversion %s does not match its recorded digest", c.ID)
	}
	return data, true, nil
}

// DeleteStaged removes a staged result (best effort; an erasure sweeps the prefix anyway).
func (s *Store) DeleteStaged(ctx context.Context, key string) {
	if key == "" || s.Originals == nil {
		return
	}
	if err := s.Originals.Delete(context.WithoutCancel(ctx), key); err != nil && s.Log != nil {
		s.Log.Error("a staged conversion result could not be deleted", "key", key, "error", err)
	}
}

// ConversionResultKey is the staged object of the running attempt ("" if none).
func (s *Store) ConversionResultKey(ctx context.Context, c Conversion) string {
	var key string
	_ = s.InTenant(ctx, c.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(result_key, '') FROM knowledge_conversions WHERE id = $1`, c.ID).Scan(&key)
	})
	return key
}

// ---- the commit --------------------------------------------------------------------------------------------------------

// ErrNotCommittable: the version the conversion was for is gone or has another original now (erased, revoked, replaced).
var ErrNotCommittable = errors.New("the document no longer has the original this conversion was made from")

// Committed says what a commit wrote.
type Committed struct {
	State  string // succeeded or truncated
	Chunks int
}

// CommitConversion replaces the text of the version with the converted text, in one transaction: its chunks (with their
// locators, flags and index text) are deleted and written anew, the projection of the vectors is queued, a pending version
// comes into force, and the job ends. Until this commits, the version has the text it had (none that is searched, for a
// pending one) and the one it would replace stays current, so half a document is never searchable (D64). text has had the
// converter's description markers taken out (cmi.CleanExtracted).
func (s *Store) CommitConversion(ctx context.Context, c Conversion, text string, info cmi.ExtractInfo) (*Committed, error) {
	parts := cmi.ChunkKnowledge(text)
	if len(parts) == 0 {
		return nil, fail("INVALID", "the converted text produced no chunks")
	}
	// the injection scan (D12) reads every chunk; it runs here, not while the base row is locked
	chunkFlags := make([][]string, len(parts))
	var docFlags []string
	for i, part := range parts {
		chunkFlags[i] = guard.Scan(part.Scanned())
		docFlags = guard.Union(docFlags, chunkFlags[i])
	}
	if docFlags == nil {
		docFlags = []string{}
	}
	var out Committed
	err := s.InTenant(ctx, c.Tenant, func(tx pgx.Tx) error {
		var baseID, title, status, source, uri, original string
		var erased bool
		var validFrom *time.Time
		var oldBytes, origBytes int64
		var revision int
		var key string
		err := tx.QueryRow(ctx, `SELECT d.knowledge_base_id::text, d.title, d.status, d.text_source, d.uri FROM knowledge_documents d WHERE d.id = $1`, c.DocumentID).
			Scan(&baseID, &title, &status, &source, &uri)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotCommittable
		}
		if err != nil {
			return err
		}
		// the same order of locks as an upload of an original: the base, then the document, then the job
		b, err := scanBaseRow(tx.QueryRow(ctx, `SELECT `+baseRowCols+` FROM knowledge_bases WHERE id = $1 FOR UPDATE`, baseID))
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT d.status, d.erasure_id IS NOT NULL, coalesce(d.original_key, ''), coalesce(d.original_sha256, ''), d.valid_from, d.byte_size::bigint, coalesce(d.original_byte_size, 0)::bigint, d.revision
			FROM knowledge_documents d WHERE d.id = $1 FOR UPDATE`, c.DocumentID).Scan(&status, &erased, &key, &original, &validFrom, &oldBytes, &origBytes, &revision); err != nil {
			return err
		}
		if erased || key != c.OriginalKey || original != c.OriginalSHA256 || (status != "pending" && status != "active" && status != "superseded") {
			return ErrNotCommittable
		}
		var jobState string
		var attempts int
		if err := tx.QueryRow(ctx, `SELECT state, attempts FROM knowledge_conversions WHERE id = $1 FOR UPDATE`, c.ID).Scan(&jobState, &attempts); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrConversionLost
			}
			return err
		}
		if jobState != ConvRunning || attempts != c.Attempts {
			return ErrConversionLost
		}
		if err := s.checkConversionQuota(ctx, tx, b, uri, c.DocumentID, int64(len(text)), oldBytes, validFrom); err != nil {
			return err
		}
		indexes := make([]string, len(parts))
		for i, part := range parts {
			indexes[i] = part.IndexText(title, s.Knowledge.IndexContext)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM knowledge_chunks WHERE document_id = $1`, c.DocumentID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `UPDATE knowledge_documents SET status = CASE WHEN status = 'pending' THEN 'active' ELSE status END, text_source = 'extracted',
				content_sha256 = $2, byte_size = $3, chunk_count = $4, flags = $5, revision = revision + 1 WHERE id = $1 RETURNING revision`,
			c.DocumentID, cmi.ContentSHA256(text), len(text), len(parts), docFlags).Scan(&revision); err != nil {
			return err
		}
		batch := &pgx.Batch{}
		for i, part := range parts {
			flags := chunkFlags[i]
			if flags == nil {
				flags = []string{}
			}
			queueChunk(batch, c.Tenant, c.DocumentID, i, part, indexes[i], flags, nil, nil)
		}
		for i := range parts { // D26: the vectors follow through the projection, at the revision this text has
			batch.Queue(`INSERT INTO projection_events (tenant_id, kind, object_id, seq, version, action) VALUES ($1, 'chunk', $2, $3, $4, 'upsert')`, c.Tenant, c.DocumentID, i, revision)
		}
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return err
		}
		state := ConvSucceeded
		notice := ""
		if info.Truncated {
			state = ConvTruncated
		}
		if len(info.Notices) > 0 {
			notice = joinNotices(info.Notices)
		}
		if _, err := tx.Exec(ctx, `UPDATE knowledge_conversions SET state = $2, notice = nullif($3, ''), chunk_count = $4, error_code = NULL, error_detail = NULL, lease_owner = NULL, lease_until = NULL,
			workspace_id = NULL, result_key = NULL, finished_at = now(), updated_at = now() WHERE id = $1`, c.ID, state, notice, len(parts)); err != nil {
			return err
		}
		out = Committed{State: state, Chunks: len(parts)}
		return Audit(ctx, tx, c.Tenant, c.RequestedBy, "knowledge.ingest."+state, c.DocumentID, map[string]any{"job": c.ID, "attempts": c.Attempts, "chunks": len(parts), "bytes": len(text), "notice": notice, "flags": docFlags})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func joinNotices(n []string) string {
	var b bytes.Buffer
	for i, s := range n {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s)
	}
	return b.String()
}

// checkConversionQuota decides whether the extracted text fits the base: the text replaces the version's supplied text, and a
// version that is not counted yet (pending) brings its original too and, coming into force, frees the bytes of the version of
// its uri that it replaces.
func (s *Store) checkConversionQuota(ctx context.Context, tx pgx.Tx, b *baseRow, uri, docID string, newBytes, oldBytes int64, validFrom *time.Time) error {
	quotaDocs, quotaBytes := s.effectiveQuota(b)
	if quotaDocs == 0 && quotaBytes == 0 {
		return nil
	}
	u, err := usageOf(ctx, tx, b.ID, uri, docID)
	if err != nil {
		return err
	}
	// a version waiting for its text is counted with its placeholder and its original (D64), so the text replaces the placeholder;
	// a version that comes into force frees the bytes of the other version of its uri that is in force now
	docs, bytes := u.Documents, u.Bytes+newBytes-oldBytes
	if validFrom == nil || !validFrom.After(s.now()) {
		bytes -= u.URIBytes - u.DocBytes
	}
	if quotaDocs > 0 && docs > quotaDocs {
		return fail("QUOTA_EXCEEDED", "knowledge base %s already holds %d documents", b.Name, u.Documents)
	}
	if quotaBytes > 0 && bytes > quotaBytes {
		return fail("QUOTA_EXCEEDED", "knowledge base %s is over its byte quota", b.Name)
	}
	return nil
}
