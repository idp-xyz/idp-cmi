package embedding

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// stub answers every text with a fixed vector, or fails.
type stub struct {
	mu   sync.Mutex
	vec  []float32
	err  error
	seen int
}

func (s *stub) Model() string { return "stub" }
func (s *stub) Embed(_ context.Context, texts []string) ([][]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen += len(texts)
	if s.err != nil {
		return nil, s.err
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = append([]float32(nil), s.vec...)
	}
	return out, nil
}
func (s *stub) set(vec []float32, err error) { s.mu.Lock(); s.vec, s.err = vec, err; s.mu.Unlock() }

func newGuard(q, m *stub) (*Guard, *[]string) {
	var results []string
	var mu sync.Mutex
	g := &Guard{Query: q, QueryBase: q, MainBase: m, ModelName: "stub", OnProbe: func(r string) { mu.Lock(); results = append(results, r); mu.Unlock() }}
	return g, &results
}

// The query endpoint is used only after the probe has passed, and the main endpoint is never used for a query.
func TestGuardKeepsQueriesOffAnEndpointThatHasNotPassed(t *testing.T) {
	ctx := context.Background()
	q, m := &stub{vec: []float32{1, 0, 0}}, &stub{vec: []float32{1, 0, 0}}
	g, _ := newGuard(q, m)
	if g.State() != StatePending {
		t.Fatalf("state %s", g.State())
	}
	if _, err := g.Embed(ctx, []string{"x"}); !errors.Is(err, ErrQueryEndpointUnavailable) || q.seen != 0 || m.seen != 0 {
		t.Fatalf("before the probe: %v, query endpoint saw %d, main saw %d", err, q.seen, m.seen)
	}
	if st := g.Check(ctx); st != StateOK {
		t.Fatalf("equal endpoints: %s", st)
	}
	q.seen, m.seen = 0, 0
	if v, err := g.Embed(ctx, []string{"x"}); err != nil || len(v) != 1 || q.seen != 1 || m.seen != 0 {
		t.Fatalf("after the probe: %v %v, query endpoint saw %d, main saw %d", v, err, q.seen, m.seen)
	}
}

func TestGuardRefusesADifferentDimensionOrModel(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		q, m []float32
	}{
		"dimension": {[]float32{1, 0}, []float32{1, 0, 0}},
		"model":     {[]float32{0, 1, 0}, []float32{1, 0, 0}},
		"zero":      {[]float32{0, 0, 0}, []float32{1, 0, 0}},
	} {
		q, m := &stub{vec: c.q}, &stub{vec: c.m}
		g, results := newGuard(q, m)
		if st := g.Check(ctx); st != StateMismatch || (*results)[0] != "mismatch" {
			t.Errorf("%s: %s %v", name, st, *results)
		}
		if want := map[string]string{"dimension": "dimensions", "model": "cosine", "zero": "cosine"}[name]; !strings.Contains(g.Detail(), want) {
			t.Errorf("%s: detail %q does not name %q", name, g.Detail(), want)
		}
		q.seen = 0
		if _, err := g.Embed(ctx, []string{"x"}); !errors.Is(err, ErrQueryEndpointUnavailable) || q.seen != 0 {
			t.Errorf("%s: a query reached the endpoint: %v", name, err)
		}
	}
}

// The tolerance is a cosine of 0.999: a CPU and a GPU copy of the model (about 0.9999) pass, a quantized copy (about 0.99) does not.
func TestGuardToleranceIsACosineOf0999(t *testing.T) {
	ctx := context.Background()
	base := []float32{1, 0}
	near := func(c float64) []float32 { // a unit vector at cosine c to base
		s := float32(1)
		for s*s > 1-float32(c)*float32(c) {
			s -= 0.00001
		}
		return []float32{float32(c), s}
	}
	for c, want := range map[float64]string{0.9999: StateOK, 0.9992: StateOK, 0.998: StateMismatch, 0.99: StateMismatch} {
		g, _ := newGuard(&stub{vec: near(c)}, &stub{vec: base})
		if got := g.Check(ctx); got != want {
			t.Errorf("cosine %v: %s, want %s (measured %v)", c, got, want, Cosine(near(c), base))
		}
	}
}

// A probe that cannot finish proves nothing: it does not switch off an endpoint that passed, and it does not switch on one that has not.
func TestGuardAnInconclusiveProbeChangesNothing(t *testing.T) {
	ctx := context.Background()
	q, m := &stub{vec: []float32{1, 0}}, &stub{vec: []float32{1, 0}}
	g, results := newGuard(q, m)
	m.set(nil, errors.New("timeout"))
	if st := g.Check(ctx); st != StatePending { // no reference yet and the documents endpoint is busy: not the query endpoint's fault, and tried again
		t.Fatalf("no reference, main busy: %s", st)
	}
	q.set(nil, errors.New("down"))
	if st := g.Check(ctx); st != StateUnreachable {
		t.Fatalf("the query endpoint down before any verdict: %s", st)
	}
	q.set([]float32{1, 0}, nil)
	m.set([]float32{1, 0}, nil)
	if g.Check(ctx) != StateOK {
		t.Fatal("not ok")
	}
	q.set(nil, &StatusError{Status: 503})
	if st := g.Check(ctx); st != StateOK {
		t.Fatalf("passed, then the query endpoint failed the probe: %s", st)
	}
	q.set([]float32{0, 1}, nil)
	m.set(nil, errors.New("busy")) // the kept reference decides; the busy documents endpoint is asked (it may be stale) and cannot answer
	if g.Check(ctx) != StateMismatch {
		t.Fatal("no mismatch")
	}
	q.set(nil, errors.New("down"))
	if st := g.Check(ctx); st != StateMismatch {
		t.Fatalf("a mismatch is not cleared by an inconclusive probe: %s", st)
	}
	if want := []string{"error", "error", "ok", "error", "mismatch", "error"}; fmt.Sprint(*results) != fmt.Sprint(want) {
		t.Fatalf("results %v, want %v", *results, want)
	}
}

// D59: once the documents endpoint has answered the probe its vector is kept; from then on the documents endpoint is not
// asked, so a restart during a bulk ingest does not leave queries lexical until the ingest has drained.
func TestGuardUsesAKeptReferenceAndNeverAsksABusyDocumentsEndpoint(t *testing.T) {
	ctx := context.Background()
	q, m := &stub{vec: []float32{1, 0}}, &stub{err: errors.New("busy behind an ingest")}
	g, _ := newGuard(q, m)
	g.LoadReference = func(context.Context) ([]float32, error) { return []float32{1, 0}, nil }
	g.SaveReference = func(context.Context, []float32) error {
		t.Fatal("a reference that was kept is saved again")
		return nil
	}
	if st := g.Check(ctx); st != StateOK || m.seen != 0 {
		t.Fatalf("restart with a reference: %s, documents endpoint asked %d times", st, m.seen)
	}
	q.set([]float32{0, 1}, nil) // not the model of the reference, and the documents endpoint cannot say otherwise
	if st := g.Check(ctx); st != StateMismatch {
		t.Fatalf("%s", st)
	}
}

func TestGuardKeepsTheFirstAnswerOfTheDocumentsEndpoint(t *testing.T) {
	ctx := context.Background()
	q, m := &stub{vec: []float32{1, 0}}, &stub{err: errors.New("busy")}
	g, _ := newGuard(q, m)
	var saved [][]float32
	g.LoadReference = func(context.Context) ([]float32, error) { return nil, nil }
	g.SaveReference = func(_ context.Context, v []float32) error { saved = append(saved, v); return nil }
	for i := 0; i < 2; i++ { // no reference and a busy documents endpoint: pending, and it is retried
		if st := g.Check(ctx); st != StatePending || len(saved) != 0 {
			t.Fatalf("round %d: %s, saved %d", i, st, len(saved))
		}
	}
	m.set([]float32{1, 0}, nil)
	if st := g.Check(ctx); st != StateOK || len(saved) != 1 {
		t.Fatalf("%s, saved %d", st, len(saved))
	}
	m.set(nil, errors.New("busy again"))
	asked := m.seen
	if st := g.Check(ctx); st != StateOK || m.seen != asked {
		t.Fatalf("%s: the documents endpoint was asked again", st)
	}
}

// A kept reference that no longer matches (the dimensions were changed in the configuration) is replaced by a fresh answer
// of the documents endpoint, not trusted.
func TestGuardReplacesAStaleReference(t *testing.T) {
	ctx := context.Background()
	q, m := &stub{vec: []float32{1, 0}}, &stub{vec: []float32{1, 0}}
	g, _ := newGuard(q, m)
	var saved [][]float32
	g.LoadReference = func(context.Context) ([]float32, error) { return []float32{1, 0, 0}, nil }
	g.SaveReference = func(_ context.Context, v []float32) error { saved = append(saved, v); return nil }
	if st := g.Check(ctx); st != StateOK || len(saved) != 1 || len(saved[0]) != 2 {
		t.Fatalf("%s %v", st, saved)
	}
}

// An error of the query endpoint is the caller's to answer lexically; nothing about it is permanent (D58 is about documents).
func TestGuardQueryEndpointFailureIsReturnedNotPermanent(t *testing.T) {
	ctx := context.Background()
	q, m := &stub{vec: []float32{1, 0}}, &stub{vec: []float32{1, 0}}
	g, _ := newGuard(q, m)
	g.Check(ctx)
	q.set(nil, &StatusError{Status: 413})
	_, err := g.Embed(ctx, []string{"x"})
	if err == nil || errors.Is(err, ErrQueryEndpointUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestCountedTellsTheRole(t *testing.T) {
	got := map[string]int{}
	ok := &stub{vec: []float32{1}}
	c := Counted{Base: ok, Role: RoleQuery, On: func(role, outcome string) { got[role+"/"+outcome]++ }}
	_, _ = c.Embed(context.Background(), []string{"a"})
	ok.set(nil, errors.New("x"))
	_, _ = c.Embed(context.Background(), []string{"a"})
	if got["query/ok"] != 1 || got["query/error"] != 1 {
		t.Fatalf("%v", got)
	}
}

func TestFailureKindsAndLastSuccess(t *testing.T) {
	for err, want := range map[error]string{
		errors.New("connection refused"): "unavailable", context.DeadlineExceeded: "unavailable",
		&StatusError{Status: 503}: "unavailable", &StatusError{Status: 429}: "unavailable",
		&StatusError{Status: 401}: "refused", &StatusError{Status: 404}: "refused",
		&StatusError{Status: 413}: "", &StatusError{Status: 400}: "", nil: "",
	} {
		if got := FailureKind(err); got != want {
			t.Errorf("%v: %q, want %q", err, got, want)
		}
	}
	var kinds []string
	seen := &Seen{}
	ok := &stub{vec: []float32{1}}
	c := Counted{Base: ok, Role: RoleDocuments, OnFailure: func(role, kind string) { kinds = append(kinds, role+"/"+kind) }, Seen: seen}
	if seen.Unix(RoleDocuments) != 0 {
		t.Fatal("a success before any request")
	}
	_, _ = c.Embed(context.Background(), []string{"a"})
	if seen.Unix(RoleDocuments) == 0 || seen.Unix(RoleQuery) != 0 {
		t.Fatalf("%v %v", seen.Unix(RoleDocuments), seen.Unix(RoleQuery))
	}
	last := seen.Unix(RoleDocuments)
	ok.set(nil, errors.New("connection refused"))
	_, _ = c.Embed(context.Background(), []string{"a"})
	ok.set(nil, &StatusError{Status: 413})
	_, _ = c.Embed(context.Background(), []string{"a"})
	if fmt.Sprint(kinds) != "[documents/unavailable]" || seen.Unix(RoleDocuments) != last {
		t.Fatalf("%v, last success moved: %v", kinds, seen.Unix(RoleDocuments) != last)
	}
}
