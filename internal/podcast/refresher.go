package podcast

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Refresher keeps subscribed shows up to date in the background.
type Refresher struct {
	store       *Store
	concurrency int
	poll        time.Duration
}

// NewRefresher creates a refresher that fetches up to concurrency feeds at once.
func NewRefresher(store *Store, concurrency int) *Refresher {
	if concurrency < 1 {
		concurrency = 4
	}
	return &Refresher{store: store, concurrency: concurrency, poll: 30 * time.Second}
}

// Run loops until ctx is cancelled.
func (r *Refresher) Run(ctx context.Context) {
	slog.Info("feed refresher started", "interval", r.store.interval, "concurrency", r.concurrency)
	for ctx.Err() == nil {
		n := r.runOnce(ctx)
		if n == 0 { // idle: wait; busy: go straight back for more
			select {
			case <-ctx.Done():
			case <-time.After(r.poll):
			}
		}
	}
}

// runOnce refreshes one batch of due shows and returns how many it handled.
func (r *Refresher) runOnce(ctx context.Context) int {
	ids, err := r.store.ClaimDue(ctx, r.concurrency*2)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("claim due shows", "err", err)
		}
		return 0
	}
	sem := make(chan struct{}, r.concurrency)
	var wg sync.WaitGroup
	for _, id := range ids {
		sem <- struct{}{}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			fctx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			if err := r.store.Refresh(fctx, id); err != nil && ctx.Err() == nil {
				slog.Error("refresh show", "show", id, "err", err)
			}
		}(id)
	}
	wg.Wait()
	return len(ids)
}
