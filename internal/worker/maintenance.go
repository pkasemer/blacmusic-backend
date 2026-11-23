package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/media"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/storage"
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
	ffmpeg      string
}

// NewMaintenance creates the maintenance loop. expireAfter should exceed the
// presigned upload URL lifetime.
func NewMaintenance(pool *pgxpool.Pool, uploads *upload.Store, releases *release.Store, blobs Blobs, expireAfter time.Duration, ffmpeg string) *Maintenance {
	return &Maintenance{pool: pool, uploads: uploads, releases: releases, blobs: blobs, expireAfter: expireAfter, ffmpeg: ffmpeg}
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

	stale, err := m.uploads.ExpirePending(ctx, m.expireAfter)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("expire pending uploads", "err", err)
		}
	}
	for _, st := range stale {
		if st.MultipartID != "" { // the parts of an unfinished multipart upload cost storage until aborted
			if err := m.blobs.AbortMultipart(ctx, st.Key, st.MultipartID); err != nil {
				slog.Error("abort multipart upload", "key", st.Key, "err", err)
			}
		}
		_ = m.blobs.Remove(ctx, st.Key)
	}
	if len(stale) > 0 {
		slog.Info("expired abandoned uploads", "count", len(stale))
	}

	m.backfillLoudness(ctx, 3)

	if n, err := image.ExpirePending(ctx, m.pool, m.blobs, 2*time.Hour); err != nil {
		if ctx.Err() == nil {
			slog.Error("expire pending images", "err", err)
		}
	} else if n > 0 {
		slog.Info("expired abandoned images", "count", n)
	}
}

// backfillLoudness measures tracks that were created before loudness existed,
// a few per run, from their high-quality rendition (small, and within a
// fraction of a dB of the original). A track that cannot be measured is marked
// as checked so it is not retried forever; a storage outage is retried later.
func (m *Maintenance) backfillLoudness(ctx context.Context, n int) {
	rows, err := m.pool.Query(ctx, `
		SELECT t.id, r.media_key FROM tracks t
		JOIN track_renditions r ON r.track_id = t.id AND r.quality = 'high'
		WHERE t.loudness_checked_at IS NULL
		ORDER BY t.created_at LIMIT $1`, n)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("loudness backfill: list", "err", err)
		}
		return
	}
	type item struct{ id, key string }
	var todo []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.key); err != nil {
			rows.Close()
			return
		}
		todo = append(todo, it)
	}
	rows.Close()

	for _, it := range todo {
		loud, retry := m.measureStored(ctx, it.key)
		if retry {
			continue
		}
		var lufs, peak, lra *float64
		if loud != nil {
			lufs, peak, lra = &loud.IntegratedLUFS, &loud.TruePeakDB, &loud.RangeLU
		}
		if _, err := m.pool.Exec(ctx, `
			UPDATE tracks SET loudness_lufs = $2, true_peak_db = $3, loudness_range_lu = $4, loudness_checked_at = now()
			WHERE id = $1`, it.id, lufs, peak, lra); err != nil && ctx.Err() == nil {
			slog.Error("loudness backfill: save", "track", it.id, "err", err)
		}
	}
}

// measureStored downloads an object and measures it. retry is true when the
// failure was the storage's, not the file's.
func (m *Maintenance) measureStored(ctx context.Context, key string) (loud *media.Loudness, retry bool) {
	dir, err := os.MkdirTemp("", "blac-loud-*")
	if err != nil {
		return nil, true
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "audio")
	if err := m.blobs.Download(ctx, key, path); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, false // the file is gone: nothing to measure, do not retry
		}
		return nil, true
	}
	mctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	loud, err = media.MeasureLoudness(mctx, m.ffmpeg, path)
	if err != nil {
		slog.Warn("loudness backfill: measure", "key", key, "err", err)
		return nil, ctx.Err() != nil
	}
	return loud, false
}
