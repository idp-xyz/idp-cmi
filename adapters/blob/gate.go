package blob

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"time"
)

// CheckTimeout bounds one attempt to reach the store and check its bucket (as idp-workspace's 20 s check).
const CheckTimeout = 20 * time.Second

// Gate puts a store behind "reachable yet?": until EnsureBucket has succeeded once, every call answers
// ErrUnavailable. The service starts without waiting for the store, so an optional store that is down or
// slow to start does not keep the service from serving; originals report unavailable until it is there.
type Gate struct {
	S3    *S3
	ready atomic.Bool
}

// Ready reports whether the store has been reached.
func (g *Gate) Ready() bool { return g.ready.Load() }

// Run tries to reach the store every interval until it succeeds or ctx ends. Start it in a goroutine.
func (g *Gate) Run(ctx context.Context, log *slog.Logger, interval time.Duration) {
	for {
		cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
		err := g.S3.EnsureBucket(cctx)
		cancel()
		if err == nil {
			g.ready.Store(true)
			if log != nil {
				log.Info("knowledge originals are available", "endpoint", g.S3.Endpoint.String(), "bucket", g.S3.Bucket)
			}
			return
		}
		if log != nil {
			log.Warn("the knowledge originals store is not usable yet; originals answer 503 until it is", "error", err, "retryIn", interval.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Put implements the store interface of the postgres adapter.
func (g *Gate) Put(ctx context.Context, key string, data []byte) error {
	if !g.Ready() {
		return ErrUnavailable
	}
	return g.S3.Put(ctx, key, data)
}

// Get implements the store interface of the postgres adapter.
func (g *Gate) Get(ctx context.Context, key string, max int64) ([]byte, error) {
	if !g.Ready() {
		return nil, ErrUnavailable
	}
	return g.S3.Get(ctx, key, max)
}

// Delete implements the store interface of the postgres adapter.
func (g *Gate) Delete(ctx context.Context, key string) error {
	if !g.Ready() {
		return ErrUnavailable
	}
	return g.S3.Delete(ctx, key)
}

// Open implements the optional streaming interface of the postgres adapter.
func (g *Gate) Open(ctx context.Context, key string, max int64) (io.ReadCloser, error) {
	if !g.Ready() {
		return nil, ErrUnavailable
	}
	return g.S3.Open(ctx, key, max)
}

// ListKeys implements the store interface of the postgres adapter.
func (g *Gate) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	if !g.Ready() {
		return nil, ErrUnavailable
	}
	return g.S3.ListKeys(ctx, prefix)
}
