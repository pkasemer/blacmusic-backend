// Command worker transcodes uploaded audio into the low/medium/high ladder.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/blacmusic/backend/internal/config"
	"github.com/blacmusic/backend/internal/db"
	"github.com/blacmusic/backend/internal/storage"
	"github.com/blacmusic/backend/internal/upload"
	"github.com/blacmusic/backend/internal/worker"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil { // safe to race the API: migrations take an advisory lock
		return err
	}

	store, err := storage.New(cfg)
	if err != nil {
		return err
	}
	if err := store.EnsureBucket(ctx); err != nil {
		return err
	}

	w := worker.New(upload.NewStore(pool), store, worker.Config{
		FFmpeg:           cfg.FFmpegPath,
		FFprobe:          cfg.FFprobePath,
		MaxBytes:         cfg.UploadMaxBytes,
		MaxDuration:      cfg.MaxTrackDuration,
		TranscodeTimeout: cfg.TranscodeTimeout,
		PollInterval:     cfg.WorkerPollInterval,
		Concurrency:      cfg.WorkerConcurrency,
	})
	slog.Info("worker started", "concurrency", cfg.WorkerConcurrency)
	w.Run(ctx)
	slog.Info("worker stopped")
	return nil
}
