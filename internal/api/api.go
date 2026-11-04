// Package api is the CMI's HTTP surface (ADR-0001 D5–D7): administration of sources, documents and
// the tenant policy; memory writes and revocations; context resolution and package read-back.
// Every request carries a bearer token; tenant isolation is the database's (RLS).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"idp-cmi/adapters/identity"
	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/authorization"
	"idp-cmi/internal/cmi"
	cmictx "idp-cmi/internal/context"
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
	Log      *slog.Logger
	Metrics  *observability.Metrics
	Version  string

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
		var in struct {
			cmi.Document
			Text string `json:"text"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.CreateDocument(r.Context(), p.Tenant, in.Document, in.Text, p.Subject)
		return http.StatusCreated, out, err
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
		out, err := a.Store.RevokeDocument(r.Context(), p.Tenant, r.PathValue("id"), p.Subject, r.URL.Query().Get("reason"))
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

	// ---- memory (writers) ----
	mux.Handle("POST /v1/memory/records", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in cmi.MemoryRecord
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		v, err := a.visibility(r.Context(), p)
		if err != nil {
			return 0, nil, err
		}
		out, err := a.Store.CreateMemory(r.Context(), p.Tenant, in, p.Subject, v.Ceiling)
		return http.StatusCreated, out, err
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
		scope := cmi.Scope{Kind: q.Get("scopeKind"), ID: q.Get("scopeId")}
		if scope.Kind == "" {
			scope = cmi.Scope{Kind: cmi.ScopeSubject, ID: p.Subject}
		}
		if err := scope.Validate(); err != nil {
			return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID", err.Error())
		}
		v, err := a.visibility(r.Context(), p)
		if err != nil {
			return 0, nil, err
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		out, err := a.Store.ListMemory(r.Context(), p.Tenant, scope, v, limit)
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
		n, err := a.Store.RevokeMemory(r.Context(), p.Tenant, in.RecordID, in.Ref, p.Subject, in.Reason, p.Has(admin))
		return 200, map[string]any{"revoked": n}, err
	}, writer))

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
		return 200, map[string]any{"packageId": r.PathValue("id"), "digest": out.Envelope.Digest, "package": out.Envelope.Document, "stale": out.Stale, "staleItems": out.StaleIDs, "expiresAt": out.ExpiredAt}, nil
	}, reader, writer))

	var h http.Handler = a.authenticate(mux)
	if a.Metrics != nil {
		h = observability.HTTPMetrics(a.Metrics, "idp_cmi", h)
	}
	return h
}
