// Package worker processes queued uploads: it validates the audio, transcodes
// it into the quality ladder, stores the renditions and creates the track.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/blacmusic/backend/internal/media"
	"github.com/blacmusic/backend/internal/storage"
	"github.com/blacmusic/backend/internal/upload"
)

// Blobs is the slice of object storage the worker needs.
type Blobs interface {
	Stat(ctx context.Context, key string) (int64, error)
	Download(ctx context.Context, key, dst string) error
	UploadFile(ctx context.Context, key, src, contentType string) error
	Remove(ctx context.Context, key string) error
}

// Config tunes the worker.
type Config struct {
	FFmpeg           string
	FFprobe          string
	MaxBytes         int64
	MaxDuration      time.Duration
	TranscodeTimeout time.Duration
	PollInterval     time.Duration
	Concurrency      int
	MaxAttempts      int           // tries before a job is failed for good (default 3)
	StaleAfter       time.Duration // a job processing longer than this is presumed dead (default 45m)
}

// Worker pulls jobs from the upload queue.
type Worker struct {
	store *upload.Store
	blobs Blobs
	cfg   Config
}

// New creates a worker.
func New(store *upload.Store, blobs Blobs, cfg Config) *Worker {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 3
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = cfg.TranscodeTimeout + 15*time.Minute
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	return &Worker{store: store, blobs: blobs, cfg: cfg}
}

// Run processes jobs until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < w.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx)
		}()
	}
	wg.Wait()
}

func (w *Worker) loop(ctx context.Context) {
	for ctx.Err() == nil {
		job, err := w.store.Claim(ctx, w.cfg.StaleAfter, w.cfg.MaxAttempts)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("claim upload", "err", err)
			}
			sleep(ctx, 5*time.Second)
			continue
		}
		if job == nil {
			sleep(ctx, w.cfg.PollInterval)
			continue
		}
		w.handle(ctx, job)
	}
}

func (w *Worker) handle(ctx context.Context, u *upload.Upload) {
	log := slog.With("upload", u.ID, "track", u.TrackID, "attempt", u.Attempts)
	start := time.Now()
	err := w.Process(ctx, u)

	// Bookkeeping must survive a shutdown signal.
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	var invalid *media.InvalidMediaError
	switch {
	case err == nil:
		log.Info("upload processed", "took", time.Since(start).Round(time.Millisecond))
	case errors.As(err, &invalid):
		log.Warn("upload rejected", "reason", invalid.Msg)
		if ferr := w.store.Fail(bg, u.ID, invalid.Msg); ferr != nil {
			log.Error("mark failed", "err", ferr)
		}
	case ctx.Err() != nil: // shutting down: let another worker take it
		log.Info("upload interrupted by shutdown")
		_ = w.store.Requeue(bg, u.ID)
	case u.Attempts >= w.cfg.MaxAttempts:
		log.Error("upload failed for good", "err", err)
		if ferr := w.store.Fail(bg, u.ID, "processing failed, please upload again"); ferr != nil {
			log.Error("mark failed", "err", ferr)
		}
	default:
		log.Error("upload failed, will retry", "err", err)
		_ = w.store.Requeue(bg, u.ID)
	}
}

// Process turns one claimed upload into a track. A *media.InvalidMediaError
// means the file is bad and retrying is pointless; any other error is retryable.
func (w *Worker) Process(ctx context.Context, u *upload.Upload) error {
	dir, err := os.MkdirTemp("", "blac-transcode-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	size, err := w.blobs.Stat(ctx, u.OriginalKey)
	if errors.Is(err, storage.ErrNotFound) {
		return &media.InvalidMediaError{Msg: "the uploaded file is missing, please upload again"}
	}
	if err != nil {
		return fmt.Errorf("stat original: %w", err)
	}
	// The presigned URL stays valid for a while after "complete", so re-check.
	if size < 1 || (w.cfg.MaxBytes > 0 && size > w.cfg.MaxBytes) {
		return &media.InvalidMediaError{Msg: "the uploaded file has an unacceptable size"}
	}

	in := filepath.Join(dir, "original")
	if err := w.blobs.Download(ctx, u.OriginalKey, in); err != nil {
		return fmt.Errorf("download original: %w", err)
	}

	info, err := media.Probe(ctx, w.cfg.FFprobe, in, w.cfg.MaxDuration)
	if err != nil {
		return err
	}

	tctx, cancel := context.WithTimeout(ctx, w.cfg.TranscodeTimeout)
	defer cancel()
	outs, err := media.Transcode(tctx, w.cfg.FFmpeg, in, dir, info)
	if err != nil {
		return err
	}

	// Raw streams (e.g. .aac) have no duration until they are re-muxed, so the
	// authoritative length comes from the finished medium rendition.
	var medium string
	for _, o := range outs {
		if o.Quality == media.Medium {
			medium = o.Path
		}
	}
	outInfo, err := media.Probe(ctx, w.cfg.FFprobe, medium, 0)
	if err != nil {
		return fmt.Errorf("probe output: %w", err)
	}
	if outInfo.Duration <= 0 {
		return fmt.Errorf("output has no duration")
	}
	if w.cfg.MaxDuration > 0 && outInfo.Duration > w.cfg.MaxDuration {
		return &media.InvalidMediaError{Msg: fmt.Sprintf("too long (the limit is %s)", w.cfg.MaxDuration.Round(time.Minute))}
	}

	res := upload.Result{DurationMs: int(outInfo.Duration.Milliseconds())}
	for _, o := range outs {
		key := fmt.Sprintf("tracks/%s/%s.%s", u.TrackID, o.Quality, media.Container)
		if err := w.blobs.UploadFile(ctx, key, o.Path, media.MimeType); err != nil {
			return fmt.Errorf("upload %s: %w", o.Quality, err)
		}
		res.Renditions = append(res.Renditions, upload.Rendition{
			Quality: string(o.Quality), MediaKey: key, BitrateKbps: o.BitrateKbps, SizeBytes: o.SizeBytes,
		})
	}
	if err := w.store.Finish(ctx, u, res); err != nil {
		return fmt.Errorf("finish: %w", err)
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
