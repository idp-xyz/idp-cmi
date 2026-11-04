// Package api is the CMI's HTTP surface (ADR-0001 D5–D7): administration of sources, documents and
// the tenant policy; memory writes and revocations; context resolution and package read-back.
// Every request carries a bearer token; tenant isolation is the database's (RLS).
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"idp-cmi/adapters/identity"
	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/authorization"
	"idp-cmi/internal/cmi"
	"idp-cmi/internal/consolidate"
	cmictx "idp-cmi/internal/context"
	"idp-cmi/internal/evaluation"
	"idp-cmi/internal/ingest"
	"idp-cmi/internal/observability"
	"idp-cmi/internal/transport"
)

// MaxBody bounds request bodies (documents may be large).
const MaxBody = 8 << 20

// Verifier authenticates bearer tokens.
type Verifier interface {
	Verify(ctx context.Context, token string) (authorization.Principal, error)
}

// API serves the CMI.
type API struct {
	Store    *postgres.Store
	Auth     Verifier
	Resolver *cmictx.Resolver
	// M3 (ADR-0001 D15–D17); nil disables the corresponding administrative endpoints (503).
	Consolidator *consolidate.Consolidator
	Ingester     *ingest.Ingester
	Evaluator    *evaluation.Suite
	Log          *slog.Logger
	Metrics      *observability.Metrics
	Version      string

	resolves *observability.CounterVec
}

type ctxKey int

const principalKey ctxKey = 1

func principalOf(r *http.Request) authorization.Principal {
	p, _ := r.Context().Value(principalKey).(authorization.Principal)
	return p
}

var open = map[string]bool{"/healthz": true, "/readyz": true, "/version": true}

func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if open[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		tok, err := identity.Bearer(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			transport.WriteProblem(w, transport.Fail(http.StatusUnauthorized, "UNAUTHENTICATED", "bearer token required"))
			return
		}
		p, err := a.Auth.Verify(r.Context(), tok)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			transport.WriteProblem(w, transport.Fail(http.StatusUnauthorized, "UNAUTHENTICATED", "invalid token"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

type handler func(r *http.Request, p authorization.Principal) (int, any, error)

// withHeaders is a body with response headers (idempotent replays say so).
type withHeaders struct {
	body    any
	headers map[string]string
}

// ifMatch reads the expected version from If-Match (D19); 0 when absent.
func ifMatch(r *http.Request) (int, error) {
	v := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, transport.Fail(http.StatusUnprocessableEntity, "INVALID", "If-Match must be a positive version number")
	}
	return n, nil
}

// idempotent runs write under the request's Idempotency-Key when there is one (D19). The request
// hash covers the path and the raw body, so a replay with another body is a conflict.
func (a *API) idempotent(r *http.Request, p authorization.Principal, raw []byte, write func() (int, any, error)) (int, any, error) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		return write()
	}
	sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), raw...))
	status, body, replayed, err := a.Store.Idempotent(r.Context(), p.Tenant, p.Subject, key, hex.EncodeToString(sum[:]), write)
	if err != nil {
		return 0, nil, err
	}
	if replayed {
		return http.StatusOK, &withHeaders{body: body, headers: map[string]string{"Idempotent-Replayed": "true"}}, nil
	}
	return status, body, nil
}

// readBody reads a bounded JSON body once (so it can be hashed and decoded).
func readBody(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, MaxBody))
	if err != nil {
		return nil, transport.Fail(http.StatusBadRequest, "BAD_REQUEST", "body unreadable: "+err.Error())
	}
	return raw, nil
}

func decodeRaw(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return transport.Fail(http.StatusBadRequest, "BAD_REQUEST", "invalid JSON body: "+err.Error())
	}
	return nil
}

// role requires one of the roles; an administrator may do everything.
func (a *API) role(h handler, roles ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalOf(r)
		ok := p.Has(authorization.RoleAdmin)
		for _, role := range roles {
			if p.Has(role) {
				ok = true
			}
		}
		if !ok {
			transport.WriteProblem(w, transport.Fail(http.StatusForbidden, "FORBIDDEN", "role "+roles[0]+" required"))
			return
		}
		status, body, err := h(r, p)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		if wh, ok := body.(*withHeaders); ok {
			for k, v := range wh.headers {
				w.Header().Set(k, v)
			}
			body = wh.body
		}
		if body == nil {
			w.WriteHeader(status)
			return
		}
		transport.WriteJSON(w, status, body)
	}
}

func (a *API) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var se *postgres.Error
	var pb *transport.Problem
	switch {
	case errors.As(err, &pb):
	case errors.As(err, &se):
		status := map[string]int{"NOT_FOUND": 404, "CONFLICT": 409, "FORBIDDEN": 403, "INVALID": 422, "GONE": 410}[se.Code]
		if status == 0 {
			status = 500
		}
		pb = transport.Fail(status, se.Code, se.Message)
	default:
		a.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err, "trace", observability.TraceID(r.Context()))
		pb = transport.Fail(http.StatusInternalServerError, "INTERNAL", "internal error")
	}
	transport.WriteProblem(w, pb)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, MaxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return transport.Fail(http.StatusBadRequest, "BAD_REQUEST", "invalid JSON body: "+err.Error())
	}
	return nil
}

// visibility derives the principal's view from its roles and the tenant policy (D7).
func (a *API) visibility(ctx context.Context, p authorization.Principal) (postgres.Visibility, error) {
	policy, err := a.Store.GetPolicy(ctx, p.Tenant)
	if err != nil {
		return postgres.Visibility{}, err
	}
	ceiling := cmi.SensitivityPublic
	switch {
	case p.Has(authorization.RoleAdmin):
		ceiling = cmi.SensitivityRestricted
	case p.Has(authorization.RoleWriter):
		ceiling = policy.WriterCeiling
	case p.Has(authorization.RoleReader):
		ceiling = policy.ReaderCeiling
	}
	return postgres.Visibility{Subject: p.Subject, Roles: p.Roles, Ceiling: ceiling}, nil
}

// Handler builds the router.
func (a *API) Handler() http.Handler {
	if a.Log == nil {
		a.Log = slog.Default()
	}
	if a.Metrics != nil {
		a.resolves = a.Metrics.Counter("idp_cmi_resolves_total", "Context resolutions by outcome.", "outcome")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := a.Store.Pool.Ping(ctx); err != nil {
			transport.WriteProblem(w, transport.Fail(http.StatusServiceUnavailable, "NOT_READY", "database unreachable"))
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		transport.WriteJSON(w, 200, map[string]any{"service": "idp-cmi", "version": a.Version})
	})

	admin, writer, reader := authorization.RoleAdmin, authorization.RoleWriter, authorization.RoleReader
	// ---- administration ----
	mux.Handle("POST /api/v1/sources", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in cmi.Source
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.CreateSource(r.Context(), p.Tenant, in, p.Subject)
		return http.StatusCreated, out, err
	}, admin))
	mux.Handle("GET /api/v1/sources", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ListSources(r.Context(), p.Tenant)
		return 200, map[string]any{"sources": out}, err
	}, admin, writer, reader))
	mux.Handle("GET /api/v1/sources/{ref}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetSource(r.Context(), p.Tenant, r.PathValue("ref"))
		return 200, out, err
	}, admin, writer, reader))
	mux.Handle("POST /api/v1/knowledge/documents", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		raw, err := readBody(r)
		if err != nil {
			return 0, nil, err
		}
		var in struct {
			cmi.Document
			Text string `json:"text"`
		}
		if err := decodeRaw(raw, &in); err != nil {
			return 0, nil, err
		}
		return a.idempotent(r, p, raw, func() (int, any, error) {
			out, err := a.Store.CreateDocument(r.Context(), p.Tenant, in.Document, in.Text, p.Subject)
			return http.StatusCreated, out, err
		})
	}, admin))
	mux.Handle("GET /api/v1/knowledge/documents", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := a.Store.ListDocuments(r.Context(), p.Tenant, limit)
		return 200, map[string]any{"documents": out}, err
	}, admin))
	mux.Handle("GET /api/v1/knowledge/documents/{id}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetDocument(r.Context(), p.Tenant, r.PathValue("id"))
		return 200, out, err
	}, admin))
	mux.Handle("DELETE /api/v1/knowledge/documents/{id}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		expected, err := ifMatch(r)
		if err != nil {
			return 0, nil, err
		}
		out, err := a.Store.RevokeDocument(r.Context(), p.Tenant, r.PathValue("id"), p.Subject, r.URL.Query().Get("reason"), expected)
		return 200, out, err
	}, admin))
	mux.Handle("GET /api/v1/policy", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetPolicy(r.Context(), p.Tenant)
		return 200, out, err
	}, admin))
	mux.Handle("PUT /api/v1/policy", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in postgres.Policy
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.SetPolicy(r.Context(), p.Tenant, in, p.Subject)
		return 200, out, err
	}, admin))
	mux.Handle("GET /api/v1/audit", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := a.Store.ListAudit(r.Context(), p.Tenant, limit)
		return 200, map[string]any{"events": out}, err
	}, admin))

	// ---- consolidation (D15), connectors (D16), evaluation (D17): administrators ----
	mux.Handle("POST /api/v1/memory/consolidate", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		if a.Consolidator == nil {
			return 0, nil, transport.Fail(http.StatusServiceUnavailable, "NOT_CONFIGURED", "no consolidation model configured on this CMI")
		}
		var in struct {
			Scope *cmi.Scope `json:"scope"`
		}
		if r.ContentLength != 0 {
			if err := decode(r, &in); err != nil {
				return 0, nil, err
			}
		}
		if in.Scope != nil {
			if err := in.Scope.Validate(); err != nil {
				return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID", err.Error())
			}
		}
		out, err := a.Consolidator.RunTenant(r.Context(), p.Tenant, in.Scope)
		return 200, out, err
	}, admin))
	mux.Handle("GET /api/v1/memory/consolidations", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := a.Store.ListConsolidations(r.Context(), p.Tenant, limit)
		return 200, map[string]any{"consolidations": out}, err
	}, admin))
	mux.Handle("POST /api/v1/connectors", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in postgres.Connector
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.CreateConnector(r.Context(), p.Tenant, in, p.Subject)
		return http.StatusCreated, out, err
	}, admin))
	mux.Handle("GET /api/v1/connectors", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ListConnectors(r.Context(), p.Tenant)
		return 200, map[string]any{"connectors": out}, err
	}, admin))
	mux.Handle("GET /api/v1/connectors/{ref}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetConnector(r.Context(), p.Tenant, r.PathValue("ref"))
		return 200, out, err
	}, admin))
	mux.Handle("POST /api/v1/connectors/{ref}/sync", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		if a.Ingester == nil {
			return 0, nil, transport.Fail(http.StatusServiceUnavailable, "NOT_CONFIGURED", "no ingestion worker on this CMI")
		}
		c, err := a.Store.GetConnector(r.Context(), p.Tenant, r.PathValue("ref"))
		if err != nil {
			return 0, nil, err
		}
		n, err := a.Ingester.Sync(r.Context(), p.Tenant, c, p.Subject)
		if err != nil {
			var se *postgres.Error
			if errors.As(err, &se) {
				return 0, nil, err
			}
			return 0, nil, transport.Fail(http.StatusBadGateway, "SYNC_FAILED", err.Error())
		}
		return 200, map[string]any{"queued": n}, nil
	}, admin))
	mux.Handle("GET /api/v1/connectors/{ref}/jobs", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		c, err := a.Store.GetConnector(r.Context(), p.Tenant, r.PathValue("ref"))
		if err != nil {
			return 0, nil, err
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := a.Store.ListJobs(r.Context(), p.Tenant, c.ID, limit)
		return 200, map[string]any{"jobs": out}, err
	}, admin))
	// ---- erasure (D20): administrators ----
	mux.Handle("POST /api/v1/erasures", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in postgres.ErasureSelector
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.RequestErasure(r.Context(), p.Tenant, in, p.Subject)
		return http.StatusCreated, out, err
	}, admin))
	mux.Handle("GET /api/v1/erasures", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := a.Store.ListErasures(r.Context(), p.Tenant, limit)
		return 200, map[string]any{"erasures": out}, err
	}, admin))
	mux.Handle("GET /api/v1/erasures/{id}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetErasure(r.Context(), p.Tenant, r.PathValue("id"))
		return 200, out, err
	}, admin))
	mux.Handle("POST /api/v1/evaluations", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		if a.Evaluator == nil {
			return 0, nil, transport.Fail(http.StatusServiceUnavailable, "NOT_CONFIGURED", "no evaluator on this CMI")
		}
		out, err := a.Evaluator.Run(r.Context(), p.Tenant, p.Subject)
		return http.StatusCreated, out, err
	}, admin))
	mux.Handle("GET /api/v1/evaluations", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := a.Store.ListEvaluations(r.Context(), p.Tenant, limit)
		return 200, map[string]any{"evaluations": out}, err
	}, admin))
	mux.Handle("GET /api/v1/evaluations/{id}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetEvaluation(r.Context(), p.Tenant, r.PathValue("id"))
		return 200, out, err
	}, admin))

	// ---- memory (writers) ----
	mux.Handle("POST /v1/memory/records", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		raw, err := readBody(r)
		if err != nil {
			return 0, nil, err
		}
		var in cmi.MemoryRecord
		if err := decodeRaw(raw, &in); err != nil {
			return 0, nil, err
		}
		v, err := a.visibility(r.Context(), p)
		if err != nil {
			return 0, nil, err
		}
		return a.idempotent(r, p, raw, func() (int, any, error) {
			out, err := a.Store.CreateMemory(r.Context(), p.Tenant, in, p.Subject, v.Ceiling)
			return http.StatusCreated, out, err
		})
	}, writer))
	mux.Handle("GET /v1/memory/records/{id}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		v, err := a.visibility(r.Context(), p)
		if err != nil {
			return 0, nil, err
		}
		out, err := a.Store.GetMemory(r.Context(), p.Tenant, r.PathValue("id"), v)
		return 200, out, err
	}, writer, reader))
	mux.Handle("GET /v1/memory/records", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		q := r.URL.Query()
		flagged := q.Get("flagged") == "true" // D12: the quarantine review, administrators only
		if flagged && !p.Has(admin) {
			return 0, nil, transport.Fail(http.StatusForbidden, "FORBIDDEN", "role cmi-admin required to list flagged records")
		}
		scope := cmi.Scope{Kind: q.Get("scopeKind"), ID: q.Get("scopeId")}
		if scope.Kind == "" && !flagged {
			scope = cmi.Scope{Kind: cmi.ScopeSubject, ID: p.Subject}
		}
		if scope.Kind != "" {
			if err := scope.Validate(); err != nil {
				return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID", err.Error())
			}
		}
		v, err := a.visibility(r.Context(), p)
		if err != nil {
			return 0, nil, err
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		out, err := a.Store.ListMemory(r.Context(), p.Tenant, scope, v, limit, flagged)
		return 200, map[string]any{"records": out}, err
	}, writer, reader))
	mux.Handle("POST /v1/memory/revoke", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			RecordID string `json:"recordId"`
			Ref      string `json:"ref"`
			Reason   string `json:"reason"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		expected, err := ifMatch(r)
		if err != nil {
			return 0, nil, err
		}
		n, err := a.Store.RevokeMemory(r.Context(), p.Tenant, in.RecordID, in.Ref, p.Subject, in.Reason, p.Has(admin), expected)
		return 200, map[string]any{"revoked": n}, err
	}, writer))

	// ---- entities (D13) ----
	mux.Handle("GET /v1/entities/{type}/{id}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		v, err := a.visibility(r.Context(), p)
		if err != nil {
			return 0, nil, err
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := a.Store.ListByEntity(r.Context(), p.Tenant, cmi.Entity{Type: r.PathValue("type"), ID: r.PathValue("id")}, v, limit)
		return 200, out, err
	}, reader, writer))

	// ---- context (readers) ----
	mux.Handle("POST /v1/context/resolve", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in cmictx.Request
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		v, err := a.visibility(r.Context(), p)
		if err != nil {
			return 0, nil, err
		}
		pkg, env, err := a.Resolver.Resolve(r.Context(), p.Tenant, v, in)
		if err != nil {
			if a.resolves != nil {
				a.resolves.Inc("error")
			}
			var se *postgres.Error
			if errors.As(err, &se) {
				return 0, nil, err
			}
			return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID", err.Error())
		}
		if a.resolves != nil {
			a.resolves.Inc("ok")
		}
		return 200, map[string]any{"packageId": pkg.ID, "digest": env.Digest, "package": json.RawMessage(env.Document)}, nil
	}, reader, writer))
	mux.Handle("GET /v1/context/packages/{id}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		requester := p.Subject
		if p.Has(admin) {
			requester = ""
		}
		out, err := a.Store.GetPackage(r.Context(), p.Tenant, r.PathValue("id"), requester)
		if err != nil {
			return 0, nil, err
		}
		return 200, map[string]any{"packageId": r.PathValue("id"), "digest": out.Envelope.Digest, "package": out.Envelope.Document, "stale": out.Stale, "staleItems": out.StaleIDs, "expiresAt": out.ExpiredAt, "redacted": out.Redacted}, nil
	}, reader, writer))

	var h http.Handler = a.authenticate(mux)
	if a.Metrics != nil {
		h = observability.HTTPMetrics(a.Metrics, "idp_cmi", h)
	}
	return h
}
