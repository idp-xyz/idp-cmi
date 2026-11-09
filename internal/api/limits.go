package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"idp-cmi/internal/authorization"
	"idp-cmi/internal/cmi"
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
