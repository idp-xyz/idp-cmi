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
	"github.com/jackc/pgx/v5"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"idp-cmi/adapters/identity"
	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/approvals"
	"idp-cmi/internal/authorization"
	"idp-cmi/internal/cmi"
	"idp-cmi/internal/consolidate"
	cmictx "idp-cmi/internal/context"
	"idp-cmi/internal/evaluation"
	"idp-cmi/internal/ingest"
	"idp-cmi/internal/observability"
	"idp-cmi/internal/pdp"
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
	// PDP is the external policy decision point (D23); nil = the CMI\'s own roles, ACLs and ceilings decide alone.
	PDP        *pdp.Client
	PDPOnError string // deny (default) | skip
	// Approvals verifies approval references of transitions (D30); nil = recorded unverified.
	Approvals *approvals.Client
	Log       *slog.Logger
	Metrics   *observability.Metrics
	Version   string

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
		status := map[string]int{"NOT_FOUND": 404, "CONFLICT": 409, "FORBIDDEN": 403, "INVALID": 422, "GONE": 410, "APPROVAL_REQUIRED": 409}[se.Code]
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

// authorize asks the PDP (D23) when there is one. Denied = 403 PDP_DENIED (audited); unreachable =
// 503 PDP_UNAVAILABLE unless the configuration says skip. The obligations returned narrow a read.
func (a *API) authorize(r *http.Request, p authorization.Principal, action string, res pdp.Resource, purpose string) (pdp.Obligations, error) {
	if a.PDP == nil {
		return pdp.Obligations{}, nil
	}
	d, err := a.PDP.Decide(r.Context(), pdp.Input{Tenant: p.Tenant, Principal: pdp.Principal{Subject: p.Subject, Roles: p.Roles, Client: p.Client, Actor: p.Actor}, Action: action, Resource: res, Purpose: purpose})
	if err != nil {
		if a.PDPOnError == "skip" {
			a.Log.Warn("PDP unavailable; proceeding on the CMI's own authorization", "action", action, "error", err)
			return pdp.Obligations{}, nil
		}
		a.Log.Warn("PDP unavailable; denying", "action", action, "error", err)
		return pdp.Obligations{}, transport.Fail(http.StatusServiceUnavailable, "PDP_UNAVAILABLE", "the policy decision point is unavailable")
	}
	if !d.Allow {
		_ = a.Store.InTenant(r.Context(), p.Tenant, func(tx pgx.Tx) error {
			return postgres.Audit(r.Context(), tx, p.Tenant, p.Subject, "pdp.deny", res.ID, map[string]any{"action": action, "resource": res, "purpose": purpose, "reason": d.Reason})
		})
		return pdp.Obligations{}, transport.Fail(http.StatusForbidden, "PDP_DENIED", "the policy decision point denied "+action)
	}
	return d.Obligations, nil
}

// narrow applies a PDP's obligations to a view.
func narrow(v postgres.Visibility, o pdp.Obligations) postgres.Visibility {
	if o.MaxSensitivity != "" && cmi.Within(o.MaxSensitivity, v.Ceiling) {
		v.Ceiling = o.MaxSensitivity
	}
	if len(o.ExcludeSources) > 0 {
		v.ExcludeSources = append(append([]string{}, v.ExcludeSources...), o.ExcludeSources...)
	}
	return v
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
		if _, err := a.authorize(r, p, "knowledge.write", pdp.Resource{Kind: "knowledge", Source: in.Source, Sensitivity: in.Sensitivity, Entities: in.Entities}, ""); err != nil {
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
		if _, err := a.authorize(r, p, "knowledge.revoke", pdp.Resource{Kind: "knowledge", ID: r.PathValue("id")}, ""); err != nil {
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
		if errors.Is(err, consolidate.ErrBusy) {
			return 0, nil, transport.Fail(http.StatusConflict, "CONSOLIDATION_RUNNING", err.Error())
		}
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
	// ---- unit directory (D22): administrators ----
	mux.Handle("PUT /api/v1/units", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in cmi.Unit
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.PutUnit(r.Context(), p.Tenant, in, p.Subject)
		return 200, out, err
	}, admin))
	mux.Handle("GET /api/v1/units", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ListUnits(r.Context(), p.Tenant)
		return 200, map[string]any{"units": out}, err
	}, admin))
	mux.Handle("DELETE /api/v1/units/{kind}/{id}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		return http.StatusNoContent, nil, a.Store.DeleteUnit(r.Context(), p.Tenant, r.PathValue("kind"), r.PathValue("id"), p.Subject)
	}, admin))

	// ---- projections (D26): administrators ----
	mux.Handle("GET /api/v1/projections", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ProjectionBacklog(r.Context(), p.Tenant)
		return 200, out, err
	}, admin))

	// ---- erasure (D20): administrators ----
	mux.Handle("POST /api/v1/erasures", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in postgres.ErasureSelector
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		if _, err := a.authorize(r, p, "erasure.request", pdp.Resource{Kind: "erasure", Source: in.Ref}, in.Reason); err != nil {
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
		if _, err := a.authorize(r, p, "memory.write", pdp.Resource{Kind: "memory", Scope: &in.Scope, Sensitivity: in.Sensitivity, Source: in.Provenance.Source, Entities: in.Entities}, ""); err != nil {
			return 0, nil, err
		}
		if in.Scope.IsUnit() { // D22: writing into a unit needs membership
			ok, err := a.Store.IsMember(r.Context(), p.Tenant, v, in.Scope, p.Has(admin))
			if err != nil {
				return 0, nil, err
			}
			if !ok {
				return 0, nil, transport.Fail(http.StatusForbidden, "FORBIDDEN", "not a member of "+in.Scope.Kind+":"+in.Scope.ID)
			}
		}
		return a.idempotent(r, p, raw, func() (int, any, error) {
			out, err := a.Store.CreateMemory(r.Context(), p.Tenant, in, p.Subject, v.Ceiling)
			return http.StatusCreated, out, err
		})
	}, writer))
	mux.Handle("POST /v1/memory/records/{id}/share", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			Scope  cmi.Scope `json:"scope"`
			Reason string    `json:"reason"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		v, err := a.visibility(r.Context(), p)
		if err != nil {
			return 0, nil, err
		}
		// the record's owner (its subject) or an administrator shares; the sharer must belong to the unit
		rec, err := a.Store.GetMemory(r.Context(), p.Tenant, r.PathValue("id"), v)
		if err != nil {
			return 0, nil, err
		}
		if !p.Has(admin) && !(rec.Scope.Kind == cmi.ScopeSubject && rec.Scope.ID == p.Subject) {
			return 0, nil, transport.Fail(http.StatusForbidden, "FORBIDDEN", "only the record's subject or an administrator may share it")
		}
		ok, err := a.Store.IsMember(r.Context(), p.Tenant, v, in.Scope, p.Has(admin))
		if err != nil {
			return 0, nil, err
		}
		if !ok {
			return 0, nil, transport.Fail(http.StatusForbidden, "FORBIDDEN", "not a member of "+in.Scope.Kind+":"+in.Scope.ID)
		}
		if _, err := a.authorize(r, p, "memory.share", pdp.Resource{Kind: "memory", ID: rec.ID, Scope: &in.Scope, Sensitivity: rec.Sensitivity, Source: rec.Provenance.Source}, in.Reason); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.Share(r.Context(), p.Tenant, rec.ID, in.Scope, in.Reason, p.Subject, v)
		return http.StatusCreated, out, err
	}, writer, reader))
	mux.Handle("GET /v1/units", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		v, err := a.visibility(r.Context(), p)
		if err != nil {
			return 0, nil, err
		}
		out, err := a.Store.UnitsOf(r.Context(), p.Tenant, v)
		if out == nil {
			out = []cmi.Scope{}
		}
		return 200, map[string]any{"units": out}, err
	}, writer, reader))
	mux.Handle("GET /v1/memory/records/{id}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		v, err := a.visibility(r.Context(), p)
		if err != nil {
			return 0, nil, err
		}
		o, err := a.authorize(r, p, "memory.read", pdp.Resource{Kind: "memory", ID: r.PathValue("id")}, "")
		if err != nil {
			return 0, nil, err
		}
		out, err := a.Store.GetMemory(r.Context(), p.Tenant, r.PathValue("id"), narrow(v, o))
		if err == nil {
			for _, src := range o.ExcludeSources {
				if out.Provenance.Source == src {
					return 0, nil, &postgres.Error{Code: "NOT_FOUND", Message: "memory record not found"}
				}
			}
		}
		return 200, out, err
	}, writer, reader))
	mux.Handle("GET /v1/memory/records", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		q := r.URL.Query()
		flagged := q.Get("flagged") == "true" // D12: the quarantine review, administrators only
		review := q.Get("status")             // D21: candidate | disputed review, administrators only
		if (flagged || review != "") && !p.Has(admin) {
			return 0, nil, transport.Fail(http.StatusForbidden, "FORBIDDEN", "role cmi-admin required to review flagged, candidate or disputed records")
		}
		scope := cmi.Scope{Kind: q.Get("scopeKind"), ID: q.Get("scopeId")}
		if scope.Kind == "" && !flagged && review == "" {
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
		if review != "" {
			out, err := a.Store.ReviewMemory(r.Context(), p.Tenant, scope, v, limit, review)
			return 200, map[string]any{"records": out}, err
		}
		if q.Get("validAt") != "" || q.Get("recordedAt") != "" { // D25
			var asOf cmi.AsOf
			var err error
			if s := q.Get("validAt"); s != "" {
				if asOf.Valid, err = time.Parse(time.RFC3339, s); err != nil {
					return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID", "validAt must be RFC 3339")
				}
			}
			if s := q.Get("recordedAt"); s != "" {
				if asOf.Recorded, err = time.Parse(time.RFC3339, s); err != nil {
					return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID", "recordedAt must be RFC 3339")
				}
			}
			now := time.Now()
			if err := asOf.Validate(now); err != nil {
				return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID", err.Error())
			}
			asOf.Normalize(now)
			out, err := a.Store.ListMemoryAsOf(r.Context(), p.Tenant, scope, v, limit, asOf)
			return 200, map[string]any{"records": out, "asOf": asOf}, err
		}
		out, err := a.Store.ListMemory(r.Context(), p.Tenant, scope, v, limit, flagged)
		return 200, map[string]any{"records": out}, err
	}, writer, reader))
	mux.Handle("POST /v1/memory/records/{id}/transitions", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			To          string `json:"to"`
			Reason      string `json:"reason"`
			ApprovalRef string `json:"approvalRef"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		expected, err := ifMatch(r)
		if err != nil {
			return 0, nil, err
		}
		if _, err := a.authorize(r, p, "memory.transition", pdp.Resource{Kind: "memory", ID: r.PathValue("id")}, in.To); err != nil {
			return 0, nil, err
		}
		approval := postgres.Approval{Ref: in.ApprovalRef}
		if in.ApprovalRef != "" && a.Approvals != nil { // D30: the approval system confirms the reference for this object
			ok, err := a.Approvals.Verify(r.Context(), approvals.Request{ApprovalRef: in.ApprovalRef, Tenant: p.Tenant, RecordID: r.PathValue("id"), To: in.To, Principal: p.Subject})
			switch {
			case errors.Is(err, approvals.ErrInvalid):
				return 0, nil, transport.Fail(http.StatusConflict, "APPROVAL_INVALID", err.Error())
			case err != nil:
				a.Log.Warn("approval system unavailable", "error", err)
				return 0, nil, transport.Fail(http.StatusServiceUnavailable, "APPROVAL_UNAVAILABLE", "the approval system is unavailable")
			}
			approval.Verified, approval.ApprovedBy, approval.ExpiresAt = true, ok.ApprovedBy, ok.ExpiresAt
		}
		out, err := a.Store.Transition(r.Context(), p.Tenant, r.PathValue("id"), in.To, in.Reason, approval, p.Subject, expected)
		return 200, out, err
	}, admin))
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
		if _, err := a.authorize(r, p, "memory.revoke", pdp.Resource{Kind: "memory", ID: in.RecordID}, in.Reason); err != nil {
			return 0, nil, err
		}
		if !p.Has(writer) && !p.Has(admin) {
			// a reader who is neither writer nor administrator may erase what is remembered about themselves, one record
			// at a time (D26): their own subject scope only, never by provenance reference, no version precondition
			if in.RecordID == "" || in.Ref != "" {
				return 0, nil, transport.Fail(http.StatusForbidden, "FORBIDDEN", "role cmi-writer required to revoke by reference; a reader may revoke their own records by recordId")
			}
			n, err := a.Store.RevokeOwnMemory(r.Context(), p.Tenant, in.RecordID, p.Subject, in.Reason)
			return 200, map[string]any{"revoked": n}, err
		}
		n, err := a.Store.RevokeMemory(r.Context(), p.Tenant, in.RecordID, in.Ref, p.Subject, in.Reason, p.Has(admin), expected)
		return 200, map[string]any{"revoked": n}, err
	}, writer, reader))

	// ---- entities (D13) ----
	mux.Handle("GET /v1/entities/{type}/{id}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		v, err := a.visibility(r.Context(), p)
		if err != nil {
			return 0, nil, err
		}
		o, err := a.authorize(r, p, "memory.read", pdp.Resource{Kind: "memory", Entities: []cmi.Entity{{Type: r.PathValue("type"), ID: r.PathValue("id")}}}, "")
		if err != nil {
			return 0, nil, err
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := a.Store.ListByEntity(r.Context(), p.Tenant, cmi.Entity{Type: r.PathValue("type"), ID: r.PathValue("id")}, narrow(v, o), limit)
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
		o, err := a.authorize(r, p, "context.resolve", pdp.Resource{Kind: "context", Scope: in.Scope, Scopes: in.Scopes, Entities: in.Entities}, in.Purpose)
		if err != nil {
			return 0, nil, err
		}
		v = narrow(v, o)
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

	mux.Handle("POST /v1/context/packages/{id}/verify", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		requester := p.Subject
		if p.Has(admin) {
			requester = ""
		}
		out, err := a.Store.VerifyPackage(r.Context(), p.Tenant, r.PathValue("id"), requester)
		return 200, out, err
	}, reader, writer))

	var h http.Handler = a.authenticate(mux)
	if a.Metrics != nil {
		h = observability.HTTPMetrics(a.Metrics, "idp_cmi", h)
	}
	return h
}
