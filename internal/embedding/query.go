package embedding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ADR-0001 D59: queries may have an embedding endpoint of their own, so that a query is not queued behind a bulk ingest on
// the endpoint that embeds documents. The two must serve the same model, or the query vector is compared with vectors it
// does not belong to; Guard checks that with a probe and keeps queries off the endpoint until the check has passed.

// Roles name the two sides of embedding in metrics and logs.
const (
	RoleQuery     = "query"
	RoleDocuments = "documents"
)

// Probe states of a Guard.
const (
	StatePending     = "pending"     // no probe has finished yet
	StateOK          = "ok"          // the endpoints agree: queries use the query endpoint
	StateMismatch    = "mismatch"    // they answered and the vectors differ: queries are not embedded
	StateUnreachable = "unreachable" // no probe has finished since the start, and one endpoint did not answer
)

// States lists every state, for a metric that reports one of them.
var States = []string{StatePending, StateOK, StateMismatch, StateUnreachable}

// ProbeText is the fixed text both endpoints embed. It carries no prefix: the probe compares the endpoints, not the prompts.
const ProbeText = "Parcels are collected between seven and eleven in the morning; customs duties are prepaid."

// MinProbeCosine is the least cosine similarity of the two probe vectors that counts as the same model. The same weights
// give 1.0 on the same hardware, and about 0.9999 between a CPU and a GPU (or float32 and bfloat16); a quantized copy of
// the model lands near 0.99 and a different model far below that. 0.999 accepts the first two and refuses the third.
const MinProbeCosine = 0.999

// ErrQueryEndpointUnavailable is what a Guard answers while the probe has not passed: the caller goes on without a vector.
var ErrQueryEndpointUnavailable = errors.New("embeddings: the query endpoint has not passed its probe")

// FailureKind classifies a failed request for the failure metric. "unavailable": no usable answer, so the endpoint rather than
// the text is the trouble (connection refused, timeout, 5xx, 429, malformed reply); "refused": the endpoint refused the caller
// (401, 403, 404...); "" for an input error (400, 413, 422), which says something about one text and not about the endpoint.
func FailureKind(err error) string {
	switch {
	case err == nil, Permanent(err):
		return ""
	case Refused(err):
		return "refused"
	}
	return "unavailable"
}

// Seen remembers when each role last had a successful embedding request, so that an alert can say "no successful embedding
// for N minutes". Safe for concurrent use.
type Seen struct {
	mu sync.Mutex
	at map[string]time.Time
}

// Mark records a success of role now.
func (s *Seen) Mark(role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at == nil {
		s.at = map[string]time.Time{}
	}
	s.at[role] = time.Now()
}

// Unix is the time of the last success of role in seconds since the epoch; 0 when there has been none.
func (s *Seen) Unix(role string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.at[role]; ok {
		return float64(t.UnixNano()) / 1e9
	}
	return 0
}

// Counted tells On the outcome of every request ("ok" or "error") with Role as the side it embeds for, OnFailure the kind
// (FailureKind) of every failed request, once per request, and marks Seen on success. Which endpoint failed is told apart
// by Role alone.
type Counted struct {
	Base      Embedder
	Role      string
	On        func(role, outcome string)
	OnFailure func(role, kind string)
	Seen      *Seen
}

// Model implements Embedder.
func (c Counted) Model() string { return c.Base.Model() }

// Embed implements Embedder.
func (c Counted) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	v, err := c.Base.Embed(ctx, texts)
	if err != nil {
		if c.On != nil {
			c.On(c.Role, "error")
		}
		if k := FailureKind(err); k != "" && c.OnFailure != nil {
			c.OnFailure(c.Role, k)
		}
		return v, err
	}
	if c.On != nil {
		c.On(c.Role, "ok")
	}
	if c.Seen != nil {
		c.Seen.Mark(c.Role)
	}
	return v, err
}

// Guard is the embedder of queries when they have an endpoint of their own. Embed uses Query while the probe holds and
// answers ErrQueryEndpointUnavailable otherwise, so the resolver answers from words alone. It never falls back to the main
// endpoint: that is the one a bulk ingest keeps busy, and a query waiting behind it for minutes is worse than a lexical answer.
type Guard struct {
	Query     Embedder // the query endpoint with the query prefix: what queries are embedded with
	QueryBase Embedder // the query endpoint as it is (no prefix), for the probe
	MainBase  Embedder // the documents endpoint as it is, for the probe
	// LoadReference and SaveReference keep the documents endpoint's vector of ProbeText (nil, nil = none yet) under the model
	// identity, so that the documents endpoint is asked only when there is no reference: it is the endpoint a bulk ingest keeps
	// busy. Both may be nil (then every probe asks it).
	LoadReference func(ctx context.Context) ([]float32, error)
	SaveReference func(ctx context.Context, v []float32) error
	ModelName     string // what Model() answers: the documents embedder's model, because the stored vectors are what queries meet
	Log           *slog.Logger

	Every     time.Duration // between probes while the check holds (default 5 minutes)
	Retry     time.Duration // between probes while it does not (default 30 seconds)
	Timeout   time.Duration // one probe, both endpoints together (default 30 seconds)
	MinCosine float64       // default MinProbeCosine

	// OnUse is told of a query that was kept off the query endpoint ("unavailable"); OnProbe of every probe's result
	// ("ok", "mismatch", "error").
	OnUse   func(role, outcome string)
	OnProbe func(result string)

	mu     sync.Mutex
	state  string
	detail string
	ref    []float32
}

// Model implements Embedder.
func (g *Guard) Model() string { return g.ModelName }

// Embed implements Embedder.
func (g *Guard) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if g.State() != StateOK {
		if g.OnUse != nil {
			g.OnUse(RoleQuery, "unavailable")
		}
		return nil, ErrQueryEndpointUnavailable
	}
	return g.Query.Embed(ctx, texts)
}

// State is the probe state (StatePending until the first probe has finished).
func (g *Guard) State() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == "" {
		return StatePending
	}
	return g.state
}

// Detail says why the state is what it is (empty before the first probe).
func (g *Guard) Detail() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.detail
}

// Run probes now and then periodically until ctx ends. It is meant to run in its own goroutine: starting the service does not wait for it.
func (g *Guard) Run(ctx context.Context) {
	for {
		st := g.Check(ctx)
		wait := g.Retry
		if wait <= 0 {
			wait = 30 * time.Second
		}
		if st == StateOK {
			wait = g.Every
			if wait <= 0 {
				wait = 5 * time.Minute
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Check embeds the probe text on the query endpoint and compares it with the documents endpoint's vector of the same text:
// the reference kept from an earlier answer, or, when there is none (or it does not match: it may be stale, for instance after
// a change of dimensions), a fresh answer, which then becomes the reference. The vectors must have the same dimension and a
// cosine of at least MinCosine. It sets and returns the state. When an endpoint does not answer the probe is inconclusive: a
// state that is "ok" stays so (a documents endpoint that is busy with an ingest must not switch off the query endpoint that
// exists for that very case), a mismatch stays a mismatch, and anything else stays not ok and is tried again.
func (g *Guard) Check(ctx context.Context) string {
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	qv, qerr := g.QueryBase.Embed(cctx, []string{ProbeText})
	cancel()
	if ctx.Err() != nil {
		return g.State()
	}
	if qerr != nil || len(qv) != 1 {
		if qerr == nil {
			qerr = errors.New("the query endpoint answered the probe with the wrong number of vectors")
		}
		return g.inconclusive(true, fmt.Errorf("query endpoint: %w", qerr))
	}
	ref := g.reference(ctx)
	fresh, staleDetail := false, ""
	for {
		if ref == nil {
			mctx, mcancel := context.WithTimeout(ctx, timeout)
			mv, merr := g.MainBase.Embed(mctx, []string{ProbeText})
			mcancel()
			if ctx.Err() != nil {
				return g.State()
			}
			if merr != nil || len(mv) != 1 {
				if merr == nil {
					merr = errors.New("the documents endpoint answered the probe with the wrong number of vectors")
				}
				if staleDetail != "" { // the kept reference says mismatch and the documents endpoint cannot say otherwise
					return g.set(StateMismatch, staleDetail+" (by the kept reference; the documents endpoint did not answer)", "mismatch")
				}
				return g.inconclusive(false, fmt.Errorf("documents endpoint: %w", merr))
			}
			ref, fresh = mv[0], true
			g.keep(ctx, ref)
		}
		detail, ok := g.compare(qv[0], ref)
		if ok {
			return g.set(StateOK, detail, "ok")
		}
		if fresh {
			return g.set(StateMismatch, detail, "mismatch")
		}
		ref, staleDetail = nil, detail // the kept reference does not match: ask the documents endpoint before saying so
	}
}

func (g *Guard) compare(q, ref []float32) (string, bool) {
	min := g.MinCosine
	if min <= 0 {
		min = MinProbeCosine
	}
	if len(q) != len(ref) {
		return fmt.Sprintf("the query endpoint answers %d dimensions, the documents endpoint %d", len(q), len(ref)), false
	}
	if c := Cosine(q, ref); c < min {
		return fmt.Sprintf("the probe text has cosine %.4f between the endpoints, below %.4f: they do not serve the same model", c, min), false
	}
	return "the endpoints agree on the probe text", true
}

// reference is the kept vector (nil when there is none or it cannot be read).
func (g *Guard) reference(ctx context.Context) []float32 {
	g.mu.Lock()
	r := g.ref
	g.mu.Unlock()
	if r != nil || g.LoadReference == nil {
		return r
	}
	r, err := g.LoadReference(ctx)
	if err != nil {
		if g.Log != nil {
			g.Log.Warn("the stored probe reference could not be read; the documents endpoint is asked", "error", err.Error())
		}
		return nil
	}
	g.mu.Lock()
	g.ref = r
	g.mu.Unlock()
	return r
}

func (g *Guard) keep(ctx context.Context, v []float32) {
	g.mu.Lock()
	g.ref = v
	g.mu.Unlock()
	if g.SaveReference != nil {
		if err := g.SaveReference(ctx, v); err != nil && g.Log != nil {
			g.Log.Warn("the probe reference could not be stored; it is asked for again after a restart", "error", err.Error())
		}
	}
}

// inconclusive: the probe could not finish. queryEndpoint says it was the query endpoint that did not answer (then a state that
// has never been decided is "unreachable"); when only the documents endpoint did not answer and there is no reference to go by,
// the state stays "pending": it is not the query endpoint's fault, and the probe is repeated.
func (g *Guard) inconclusive(queryEndpoint bool, err error) string {
	if g.OnProbe != nil {
		g.OnProbe("error")
	}
	g.mu.Lock()
	if queryEndpoint && (g.state == "" || g.state == StatePending) {
		g.state = StateUnreachable
	}
	st := g.state
	g.mu.Unlock()
	if st == "" {
		st = StatePending
	}
	if g.Log != nil {
		g.Log.Warn("the query endpoint could not be checked against the documents endpoint", "state", st, "error", err.Error())
	}
	return st
}

func (g *Guard) set(state, detail, result string) string {
	if g.OnProbe != nil {
		g.OnProbe(result)
	}
	g.mu.Lock()
	prev := g.state
	g.state, g.detail = state, detail
	g.mu.Unlock()
	if g.Log != nil {
		switch {
		case state == StateMismatch:
			g.Log.Error("the query endpoint does not match the documents endpoint: queries are answered from words alone until it does", "state", state, "detail", detail)
		case prev != state:
			g.Log.Info("the query endpoint matches the documents endpoint: queries use it", "state", state, "detail", detail)
		}
	}
	return state
}
