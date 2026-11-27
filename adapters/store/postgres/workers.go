package postgres

import (
	"context"
	"sync"
	"time"
)

// EmbeddingWorkers configures the two loops that keep vectors current (ADR-0001 D26, D79).
type EmbeddingWorkers struct {
	Interval      time.Duration // how often each loop runs
	ProjectLimit  int           // events per projection pass
	BackfillLimit int           // rows per backfill pass
	// Done is told the result of each pass (kind is "project" or "backfill"); may be nil.
	Done func(kind string, n int, err error)
}

// RunEmbeddingWorkers runs the projection and the backfill as two loops, each on its own ticker, and returns when ctx is done
// and both have stopped (D79). A long backfill, which with a CPU embedder can take minutes, therefore never delays the projection
// of new events. The loops cannot work the same rows twice: the projection takes events with FOR UPDATE SKIP LOCKED, the backfill
// takes rows that have no vector of the current model and writes the same vector the projection would, guarded by the row's version.
func (s *Store) RunEmbeddingWorkers(ctx context.Context, w EmbeddingWorkers) {
	if w.Interval <= 0 {
		w.Interval = 30 * time.Second
	}
	report := func(kind string, n int, err error) {
		if w.Done != nil {
			w.Done(kind, n, err)
		}
	}
	loop := func(kind string, pass func(context.Context) (int, error)) {
		t := time.NewTicker(w.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n, err := pass(ctx)
				if err != nil && ctx.Err() != nil {
					return
				}
				report(kind, n, err)
			}
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		loop("project", func(c context.Context) (int, error) { return s.Project(c, w.ProjectLimit) })
	}()
	go func() {
		defer wg.Done()
		loop("backfill", func(c context.Context) (int, error) { return s.BackfillEmbeddings(c, w.BackfillLimit) })
	}()
	wg.Wait()
}
