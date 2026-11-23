package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/upload"
)

// Maintenance runs the small periodic jobs: going live with scheduled
// releases, and clearing out uploads that were started but never finished
// (which also hands their reserved storage back to the artist).
type Maintenance struct {
	pool        *pgxpool.Pool
	uploads     *upload.Store
	releases    *release.Store
	blobs       Blobs
	expireAfter time.Duration // how long a pending upload may wait for its file
}

// NewMaintenance creates the maintenance loop. expireAfter should exceed the
// presigned upload URL lifetime.
func NewMaintenance(pool *pgxpool.Pool, uploads *upload.Store, releases *release.Store, blobs Blobs, expireAfter time.Duration) *Maintenance {
	return &Maintenance{pool: pool, uploads: uploads, releases: releases, blobs: blobs, expireAfter: expireAfter}
}

// Run loops until ctx is cancelled.
func (m *Maintenance) Run(ctx context.Context) {
	slog.Info("maintenance started")
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		m.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce performs every job once.
func (m *Maintenance) RunOnce(ctx context.Context) {
	if n, err := m.releases.PublishDue(ctx); err != nil {
		if ctx.Err() == nil {
			slog.Error("publish scheduled releases", "err", err)
		}
	} else if n > 0 {
		slog.Info("scheduled releases went live", "count", n)
	}

	keys, err := m.uploads.ExpirePending(ctx, m.expireAfter)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("expire pending uploads", "err", err)
		}
	}
	for _, k := range keys {
		_ = m.blobs.Remove(ctx, k)
	}
	if len(keys) > 0 {
		slog.Info("expired abandoned uploads", "count", len(keys))
	}

	if n, err := image.ExpirePending(ctx, m.pool, m.blobs, 2*time.Hour); err != nil {
		if ctx.Err() == nil {
			slog.Error("expire pending images", "err", err)
		}
	} else if n > 0 {
		slog.Info("expired abandoned images", "count", n)
	}
}
