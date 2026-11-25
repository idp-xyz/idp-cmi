// Package ingestfiles is CMI's internal file endpoint for the ingest sandbox (ADR-0001 D62): the one URL idp-sandbox's
// sandbox_import_file and sandbox_export_file are configured with (IDP_SANDBOX_FILES_URL).
//
//	GET  /content   the original of the ticket's document (a read ticket)
//	POST /          the converted text of the ticket's document (a write ticket)
//
// It listens on its own address, for the internal network only. The sandbox that presents a ticket verifies nothing of it,
// so everything is checked here: the signature, the audience, the expiry and the kind (internal/ticket), that the ticket was
// not presented before (persisted, so it holds across restarts and replicas), and that it is for the running attempt of a
// conversion job whose document still has the original it names (the store). Whatever is wrong, the answer is the same 403.
package ingestfiles

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync/atomic"

	"idp-cmi/adapters/blob"
	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/ticket"
)

// Backend is what the endpoint needs of the store.
type Backend interface {
	UseTicket(ctx context.Context, c ticket.Claims) (bool, error)
	ConversionOriginal(ctx context.Context, c ticket.Claims) (f *os.File, media string, size int64, err error)
	StageConversionResult(ctx context.Context, c ticket.Claims, body io.Reader, limit int64) (*postgres.StagedResult, error)
}

// DefaultMaxResult is the largest converted text accepted: the 4 MiB a document's text may have, and room for the converter's
// markers.
const DefaultMaxResult = 6 << 20

// DefaultMaxReads is how many originals are served at once.
const DefaultMaxReads = 8

// Handler serves the endpoint.
type Handler struct {
	Verifier  *ticket.Verifier
	Backend   Backend
	Log       *slog.Logger
	MaxResult int64 // 0 = DefaultMaxResult
	MaxReads  int   // 0 = DefaultMaxReads

	reads atomic.Int32
}

// ServeHTTP routes the two requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/content":
		h.read(w, r)
	case r.Method == http.MethodPost && (r.URL.Path == "/" || r.URL.Path == ""):
		h.write(w, r)
	default:
		refuse(w, http.StatusNotFound, "no such route")
	}
}

// refuse answers with a status and a message. A 5xx and a 429 carry no message: the sandbox's client (files.status) prints
// "the Workspace refused the file: <message>" whenever there is one and "the Workspace refused the file (HTTP n)" when there is
// not, and CMI's worker tells a busy or failing endpoint (HTTP 5xx, 429: try again) from a refusal (a message: do not) by that.
func refuse(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status >= 500 || status == http.StatusTooManyRequests {
		_, _ = w.Write([]byte("{}"))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg}})
}

// verified returns the claims of the request's bearer ticket when its signature, audience, expiry and kind are right. Every
// failure has been answered when it returns false. It does not use the ticket up.
func (h *Handler) verified(w http.ResponseWriter, r *http.Request, kind string) (ticket.Claims, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		refuse(w, http.StatusUnauthorized, "a ticket is required")
		return ticket.Claims{}, false
	}
	c, err := h.Verifier.Verify(tok, kind)
	if err != nil {
		h.log("file ticket refused", "kind", kind, "reason", "verification")
		refuse(w, http.StatusForbidden, "the ticket is not valid")
		return c, false
	}
	return c, true
}

// use marks the verified ticket used: a ticket works once. It reports false, having answered, when it was used before.
func (h *Handler) use(w http.ResponseWriter, r *http.Request, c ticket.Claims) bool {
	first, err := h.Backend.UseTicket(r.Context(), c)
	switch {
	case errors.Is(err, postgres.ErrTicketRefused):
		first = false
	case err != nil:
		h.log("file ticket could not be recorded", "kind", c.Kind, "job", c.Job, "error", err.Error())
		refuse(w, http.StatusServiceUnavailable, "try again later")
		return false
	}
	if !first {
		h.log("file ticket refused", "kind", c.Kind, "job", c.Job, "reason", "used")
		refuse(w, http.StatusForbidden, "the ticket is not valid")
		return false
	}
	return true
}

func (h *Handler) log(msg string, args ...any) {
	if h.Log != nil {
		h.Log.Warn(msg, args...)
	}
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request) {
	c, ok := h.verified(w, r, ticket.Read)
	if !ok {
		return
	}
	limit := int32(h.MaxReads)
	if limit <= 0 {
		limit = DefaultMaxReads
	}
	// the place is taken before the ticket is used: a ticket that was turned away because the endpoint is busy is still good
	if h.reads.Add(1) > limit {
		h.reads.Add(-1)
		refuse(w, http.StatusTooManyRequests, "too many originals are being read at once")
		return
	}
	defer h.reads.Add(-1)
	if !h.use(w, r, c) {
		return
	}
	f, media, size, err := h.Backend.ConversionOriginal(r.Context(), c)
	if err != nil {
		h.fail(w, c, err)
		return
	}
	defer f.Close()
	ext, _ := postgres.IngestExtension(media)
	if ext == "" {
		ext = "bin"
	}
	sum := strings.TrimPrefix(c.SHA256, "sha256:")
	if _, err := hex.DecodeString(sum); err != nil {
		sum = ""
	}
	hd := w.Header()
	hd.Set("Content-Type", media)
	hd.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "original." + ext}))
	hd.Set("Content-Length", strconv.FormatInt(size, 10))
	hd.Set("Idp-File-Ref", c.Document)
	hd.Set("Idp-File-Sha256", sum)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request) {
	c, ok := h.verified(w, r, ticket.Write)
	if !ok || !h.use(w, r, c) {
		return
	}
	limit := h.MaxResult
	if limit <= 0 {
		limit = DefaultMaxResult
	}
	if r.ContentLength > limit {
		refuse(w, http.StatusRequestEntityTooLarge, "the result is larger than the limit")
		return
	}
	res, err := h.Backend.StageConversionResult(r.Context(), c, r.Body, limit)
	if err != nil {
		h.fail(w, c, err)
		return
	}
	name := "result.md"
	if _, params, err := mime.ParseMediaType(r.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		if n := path.Base(params["filename"]); n != "." && n != "/" && len(n) <= 100 {
			name = n
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ref": res.Ref, "name": name, "size": res.Size, "sha256": strings.TrimPrefix(res.SHA256, "sha256:")})
}

// fail answers a store error: a ticket that no longer fits its job is the same refusal as any other; a store that is away is
// 503 (the sandbox tells the worker the file could not be moved, which it retries); the rest is 500.
func (h *Handler) fail(w http.ResponseWriter, c ticket.Claims, err error) {
	switch {
	case errors.Is(err, postgres.ErrTicketRefused):
		h.log("file ticket refused", "kind", c.Kind, "job", c.Job, "reason", "binding")
		refuse(w, http.StatusForbidden, "the ticket is not valid")
	case errors.Is(err, postgres.ErrResultTooLarge):
		refuse(w, http.StatusRequestEntityTooLarge, "the result is larger than the limit")
	case errors.Is(err, blob.ErrUnavailable) || blob.Transient(err):
		refuse(w, http.StatusServiceUnavailable, "the original store is not reachable")
	default:
		var se *postgres.Error
		if errors.As(err, &se) {
			switch se.Code {
			case "NOT_CONFIGURED":
				refuse(w, http.StatusServiceUnavailable, "the original store is not reachable")
				return
			case "INVALID":
				refuse(w, http.StatusUnprocessableEntity, se.Message)
				return
			}
		}
		h.log("file endpoint failed", "kind", c.Kind, "job", c.Job, "error", err.Error())
		refuse(w, http.StatusInternalServerError, "the file could not be served")
	}
}
