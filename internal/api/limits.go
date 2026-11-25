package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/authorization"
	"idp-cmi/internal/cmi"
	"idp-cmi/internal/embedwire"
	"idp-cmi/internal/observability"
	"idp-cmi/internal/transport"
)

// Limits and write metrics (production readiness): a runaway agent or a misconfigured writer must
// not be able to fill the store or hammer it. The limits are soft by design (see config.Limits).

// rateLimiter is a fixed one-minute window per principal, kept in memory (per replica).
type rateLimiter struct {
	mu      sync.Mutex
	windows map[string]*window
}

type window struct {
	start time.Time
	n     int
}

// allow reports whether the principal may write now, or how many seconds until the window ends.
func (l *rateLimiter) allow(key string, perMinute int, now time.Time) (ok bool, retryAfter int) {
	if perMinute <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.windows == nil {
		l.windows = map[string]*window{}
	}
	w := l.windows[key]
	if w == nil || now.Sub(w.start) >= time.Minute {
		if len(l.windows) > 10000 { // bounded: drop the expired windows
			for k, v := range l.windows {
				if now.Sub(v.start) >= time.Minute {
					delete(l.windows, k)
				}
			}
		}
		w = &window{start: now}
		l.windows[key] = w
	}
	if w.n >= perMinute {
		secs := int(time.Minute.Seconds() - now.Sub(w.start).Seconds())
		if secs < 1 {
			secs = 1
		}
		return false, secs
	}
	w.n++
	return true, 0
}

// MaxConcurrentUploads is how many originals one caller may be sending at once (D54): each is held in memory
// while it is read, hashed and written to the object store.
const MaxConcurrentUploads = 2

// uploadGate counts the uploads in flight per caller.
type uploadGate struct {
	mu sync.Mutex
	in map[string]int
}

// acquire takes a place for key and returns its release; ok is false when key has max uploads in flight.
func (g *uploadGate) acquire(key string, max int) (release func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.in == nil {
		g.in = map[string]int{}
	}
	if g.in[key] >= max {
		return nil, false
	}
	g.in[key]++
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.in[key]--; g.in[key] <= 0 {
			delete(g.in, key)
		}
	}, true
}

// DefaultConcurrentDownloads is how many originals may be downloaded at once: each download is verified through a
// temporary file of up to the size of the original before it is sent, so the scratch directory must hold this many
// of the largest (deploy/kubernetes/cmi.yaml sizes its emptyDir for it).
const DefaultConcurrentDownloads = 8

// DefaultDownloadsPerCaller is how many of those one caller may have at once.
const DefaultDownloadsPerCaller = 2

// DefaultTransferTimeout is how long one upload or download of an original may take (the server's own read and write timeouts, 60 s, do not apply to these routes).
const DefaultTransferTimeout = 10 * time.Minute

// ValidateUploadBudget checks the setting knowledge.maxInFlightUploadBytes: it has to be at least the largest original, or no upload of that type could ever
// start.
func ValidateUploadBudget(n int64) error {
	if n > 0 && n < postgres.LargestOriginal() {
		return fmt.Errorf("knowledge.maxInFlightUploadBytes is %d: it must be at least %d, the largest original an upload may be", n, postgres.LargestOriginal())
	}
	return nil
}

// DefaultUploadBytesInFlight bounds the bytes of originals being received at once, over all callers (D54): each upload holds
// its body in memory until it is written to the object store, and each reserves the largest size its type may have.
const DefaultUploadBytesInFlight = 256 << 20

// byteGate counts the bytes reserved by uploads in flight.
type byteGate struct {
	mu   sync.Mutex
	used int64
}

func (g *byteGate) acquire(n, max int64) (release func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.used+n > max {
		return nil, false
	}
	g.used += n
	return func() {
		g.mu.Lock()
		g.used -= n
		g.mu.Unlock()
	}, true
}

func (a *API) count(outcome string) {
	if a.writes != nil {
		a.writes.Inc(outcome)
	}
}

// checkWriteLimits answers 429 when the principal writes too fast or the scope or tenant is full.
func (a *API) checkWriteLimits(ctx context.Context, p authorization.Principal, scope cmi.Scope) error {
	l := a.Limits
	if ok, retry := a.limiter.allow(p.Tenant+"/"+p.Subject, l.WritesPerMinute, time.Now()); !ok {
		a.count("rate_limited")
		pb := transport.Fail(http.StatusTooManyRequests, "RATE_LIMITED", "too many memory writes; retry later")
		pb.RetryAfter = retry
		return pb
	}
	if l.MaxRecordsPerScope <= 0 && l.MaxRecordsPerTenant <= 0 {
		return nil
	}
	inScope, total, err := a.Store.LiveCounts(ctx, p.Tenant, scope.Kind, scope.ID)
	if err != nil {
		return err
	}
	if l.MaxRecordsPerScope > 0 && inScope >= l.MaxRecordsPerScope {
		a.count("quota")
		return transport.Fail(http.StatusTooManyRequests, "QUOTA_EXCEEDED", "this scope already holds the maximum number of records; revoke or let some expire")
	}
	if l.MaxRecordsPerTenant > 0 && total >= l.MaxRecordsPerTenant {
		a.count("quota")
		return transport.Fail(http.StatusTooManyRequests, "QUOTA_EXCEEDED", "the tenant already holds the maximum number of records")
	}
	return nil
}

// countWrite records a write's outcome: stored, flagged (quarantined by the injection scan) or rejected.
func (a *API) countWrite(out *cmi.MemoryRecord, err error) {
	switch {
	case err != nil:
		a.count("rejected")
	case out != nil && len(out.Flags) > 0:
		a.count("flagged")
	default:
		a.count("stored")
	}
}

// registerGauges exposes the projection backlog and the connection pool. The backlog query runs at
// scrape time with a short timeout; a failed query reports nothing rather than a wrong zero.
func (a *API) registerGauges() {
	if a.Store == nil {
		return
	}
	embedFailures := a.Metrics.Counter("idp_cmi_embed_failures_total", embedwire.FailuresHelp, "kind", "role")
	a.Store.OnEmbedFailure = func(kind string) { embedFailures.Inc(kind, "documents") }
	a.Metrics.GaugeFunc("idp_cmi_erasures_waiting_for_store", "Erasures held up because the object store of the originals cannot be reached.", nil, func() []observability.LabeledValue {
		n, _ := a.Store.WaitingErasures()
		return []observability.LabeledValue{{Value: float64(n)}}
	})
	a.Metrics.GaugeFunc("idp_cmi_erasures_waiting_oldest_seconds", "How long the longest-waiting such erasure has been waiting (0 when none).", nil, func() []observability.LabeledValue {
		_, age := a.Store.WaitingErasures()
		return []observability.LabeledValue{{Value: age}}
	})
	a.Metrics.GaugeFunc("idp_cmi_projection_pending", "Embedding projection events not yet processed (all tenants).", nil, func() []observability.LabeledValue {
		n, _, err := a.projection()
		if err != nil {
			return nil
		}
		return []observability.LabeledValue{{Value: float64(n)}}
	})
	a.Metrics.GaugeFunc("idp_cmi_projection_oldest_age_seconds", "Age of the oldest unprocessed projection event (0 when none).", nil, func() []observability.LabeledValue {
		_, age, err := a.projection()
		if err != nil {
			return nil
		}
		return []observability.LabeledValue{{Value: age}}
	})
	a.Metrics.GaugeFunc("idp_cmi_db_up", "1 when the database answers, 0 when it does not.", nil, func() []observability.LabeledValue {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if a.Store.Pool.Ping(ctx) != nil {
			return []observability.LabeledValue{{Value: 0}}
		}
		return []observability.LabeledValue{{Value: 1}}
	})
}

var errNoStore = errors.New("no store")

func (a *API) projection() (int, float64, error) {
	if a.Store == nil {
		return 0, 0, errNoStore
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return a.Store.ProjectionTotals(ctx)
}
