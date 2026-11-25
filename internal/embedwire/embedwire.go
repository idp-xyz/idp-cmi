// Package embedwire builds the embedders of the service from the configuration (ADR-0001 D11, D37, D59): documents on the
// main endpoint, queries on the main endpoint or, when embedding.query is set, on their own endpoint behind a probe. It is
// the one place that decides which side uses which endpoint, so that tests can run it.
package embedwire

import (
	"context"
	"log/slog"

	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/config"
	"idp-cmi/internal/embedding"
	"idp-cmi/internal/observability"
)

// FailuresHelp is the help text of idp_cmi_embed_failures_total.
const FailuresHelp = "Embedding failures by kind and role (documents: chunks and rows; query: the asker's text). permanent: the text was refused; transient: a chunk to be retried later; refused: the endpoint refused the credential or address; unavailable: a whole request got no usable answer (connection refused, timeout, 5xx), once per failed request, and no chunk's attempts are spent on it."

// Options are what Build needs. Main and Query are the endpoints as they are (no prefix); Query is nil without embedding.query.
type Options struct {
	Config  *config.Embedding
	Main    embedding.Embedder
	Query   embedding.Embedder
	Store   *postgres.Store // keeps the probe's reference vector; nil: the documents endpoint is asked at every probe
	Metrics *observability.Metrics
	Log     *slog.Logger
}

// Result is what the service wires in: Documents is the store's embedder, Queries the resolver's, Guard the probe (nil
// when queries use the main endpoint).
type Result struct {
	Documents embedding.Asymmetric
	Queries   embedding.Embedder
	Guard     *embedding.Guard
	Seen      *embedding.Seen
	// failures counts chunk failures the store reports (permanent, transient, refused), as role documents
	Failure func(kind string)
}

// Build wires the embedders and registers their metrics. The probe is not started: the caller runs Guard.Run in a goroutine.
func Build(o Options) *Result {
	e := o.Config
	m := o.Metrics
	if m == nil {
		m = observability.NewMetrics()
	}
	requests := m.Counter("idp_cmi_embedding_requests_total", "Embedding requests by role (query: the asker's text; documents: projection, backfill and writes) and outcome (ok, error; unavailable: a query kept off the query endpoint by its probe).", "role", "outcome")
	failures := m.Counter("idp_cmi_embed_failures_total", FailuresHelp, "kind", "role")
	seen := &embedding.Seen{}
	m.GaugeFunc("idp_cmi_embedding_last_success_timestamp_seconds", "Unix time of the last successful embedding request, by role (0: none since the start). Alert on time() minus this.", []string{"role"}, func() []observability.LabeledValue {
		return []observability.LabeledValue{
			{Labels: []string{embedding.RoleDocuments}, Value: seen.Unix(embedding.RoleDocuments)},
			{Labels: []string{embedding.RoleQuery}, Value: seen.Unix(embedding.RoleQuery)},
		}
	})
	count := func(role, outcome string) { requests.Inc(role, outcome) }
	// Whole-request failures are counted once per request, here. For documents the store counts what it knows per chunk
	// (permanent, transient, refused), so only "unavailable" comes from here; the asker's text has no one else counting.
	onFailure := func(role, kind string) {
		if role == embedding.RoleDocuments && kind != "unavailable" {
			return
		}
		failures.Inc(kind, role)
	}
	r := &Result{Seen: seen, Failure: func(kind string) { failures.Inc(kind, embedding.RoleDocuments) }}
	r.Documents = embedding.Asymmetric{Base: embedding.Counted{Base: o.Main, Role: embedding.RoleDocuments, On: count, OnFailure: onFailure, Seen: seen}, QueryPrefix: e.QueryPrefix, DocumentPrefix: e.DocumentPrefix}
	if o.Query == nil {
		r.Queries = embedding.Asymmetric{Base: embedding.Counted{Base: o.Main, Role: embedding.RoleQuery, On: count, OnFailure: onFailure, Seen: seen}, QueryPrefix: e.QueryPrefix, DocumentPrefix: e.DocumentPrefix}.Queries()
		return r
	}
	qasym := embedding.Asymmetric{Base: embedding.Counted{Base: o.Query, Role: embedding.RoleQuery, On: count, OnFailure: onFailure, Seen: seen}, QueryPrefix: e.QueryPrefix, DocumentPrefix: e.DocumentPrefix}
	probes := m.Counter("idp_cmi_query_endpoint_probes_total", "Probes of the query endpoint against the documents endpoint, by result (ok, mismatch, error).", "result")
	model := r.Documents.Model()
	g := &embedding.Guard{Query: qasym.Queries(), QueryBase: o.Query, MainBase: o.Main, ModelName: model, Log: o.Log,
		OnUse: count, OnProbe: func(res string) { probes.Inc(res) }}
	if o.Store != nil { // D59: the documents endpoint's answer is kept under the model identity of the stored vectors
		g.LoadReference = func(ctx context.Context) ([]float32, error) {
			return o.Store.ProbeReference(ctx, model, embedding.ProbeText)
		}
		g.SaveReference = func(ctx context.Context, v []float32) error {
			return o.Store.SaveProbeReference(ctx, model, embedding.ProbeText, v)
		}
	}
	m.GaugeFunc("idp_cmi_query_endpoint_state", "1 for the state of the query endpoint: ok (queries use it), pending, mismatch or unreachable (queries are answered from words alone).", []string{"state"}, func() []observability.LabeledValue {
		cur := g.State()
		out := make([]observability.LabeledValue, 0, len(embedding.States))
		for _, s := range embedding.States {
			v := 0.0
			if s == cur {
				v = 1
			}
			out = append(out, observability.LabeledValue{Labels: []string{s}, Value: v})
		}
		return out
	})
	r.Guard, r.Queries = g, g
	return r
}
