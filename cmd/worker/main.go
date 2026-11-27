// Command worker transcodes uploaded audio into the low/medium/high ladder.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/blacmusic/backend/internal/config"
	"github.com/blacmusic/backend/internal/db"
	"github.com/blacmusic/backend/internal/genre"
	"github.com/blacmusic/backend/internal/importer"
	"github.com/blacmusic/backend/internal/mailer"
	"github.com/blacmusic/backend/internal/notify"
	"github.com/blacmusic/backend/internal/podcast"
	"github.com/blacmusic/backend/internal/push"
	"github.com/blacmusic/backend/internal/recommend"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/storage"
	"github.com/blacmusic/backend/internal/sysmetrics"
	"github.com/blacmusic/backend/internal/transfer"
	"github.com/blacmusic/backend/internal/upload"
	"github.com/blacmusic/backend/internal/worker"
	"github.com/blacmusic/backend/internal/youtube"
	"github.com/blacmusic/backend/internal/ytdlp"
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
	go genre.Watch(ctx, pool, 30*time.Second) // the team manages the genres

	store, err := storage.New(cfg)
	if err != nil {
		return err
	}
	if err := store.EnsureBucket(ctx); err != nil {
		return err
	}

	uploads := upload.NewStore(pool)
	uploads.OnReady = notify.OnReleaseProcessed // the push and the email are queued in the transaction that finishes the last file
	w := worker.New(uploads, store, worker.Config{
		FFmpeg:           cfg.FFmpegPath,
		FFprobe:          cfg.FFprobePath,
		MaxBytes:         cfg.UploadMaxBytes,
		MaxDuration:      cfg.MaxTrackDuration,
		TranscodeTimeout: cfg.TranscodeTimeout,
		PollInterval:     cfg.WorkerPollInterval,
		Concurrency:      cfg.WorkerConcurrency,
	})
	refresher := podcast.NewRefresher(podcast.NewStore(pool,
		podcast.NewFetcher(cfg.FeedAllowPrivate, cfg.FeedFetchTimeout), cfg.FeedRefreshInterval), 4)

	slog.Info("worker started", "concurrency", cfg.WorkerConcurrency)
	var wg sync.WaitGroup
	releases := release.NewStore(pool, store, cfg.PublicAPIURL)
	// imports an artist runs are published for review too; the team's bulk imports use their own store below
	releases.Review, uploads.Review = cfg.ReleaseReview, cfg.ReleaseReview
	releases.OnLive = notify.OnReleaseLive // the notification is queued in the transaction that makes the release public
	// Pending uploads are abandoned once their presigned URL (plus a margin) has expired.
	maint := worker.NewMaintenance(pool, uploads, releases, store, cfg.UploadURLTTL+30*time.Minute, cfg.FFmpegPath)
	host, _ := os.Hostname()
	transferMgr := transfer.NewManager(pool, youtube.NewStore(pool), releases, uploads)
	transferMgr.OnEnd = notify.OnTransferEnded(pool)
	transferMgr.OnNeedsAudio = notify.OnNeedsAudio(pool)
	transferMgr.FetchEnabled = cfg.YoutubeAudioFetch
	var gateway push.Gateway
	if !cfg.PushFake {
		gateway = push.NewExpo(cfg.ExpoAccessToken)
	}
	pusher := push.NewSender(pool, gateway, cfg.PushFake)
	mail, err := mailer.FromConfig(&cfg)
	if err != nil {
		return err
	}
	emailer := mailer.NewSender(pool, mail)
	transfers := transfer.NewRunner(
		transferMgr, pool, store, releases,
		transfer.DefaultFetch(&http.Client{Timeout: 20 * time.Second}), fmt.Sprintf("%s-%d", host, os.Getpid()), cfg.TransferConcurrency)
	if cfg.YoutubeAudioFetch {
		transfers.SetFetcher(ytdlp.New(cfg.YtdlpPath, cfg.YtdlpJSRuntime, cfg.YtdlpMaxBytes, cfg.YtdlpMaxDuration, cfg.YtdlpTimeout))
		slog.Info("importing audio from YouTube is on", "tool", cfg.YtdlpPath)
	}
	// bulk imports get a release store without publish hooks: they send no mail
	imports := importer.NewRunner(pool, store, release.NewStore(pool, store, cfg.PublicAPIURL), uploads)
	wg.Add(10)
	go func() { defer wg.Done(); recommend.Run(ctx, pool) }()                    // song statistics the recommendation engine ranks with
	go func() { defer wg.Done(); sysmetrics.NewSampler(pool, store).Run(ctx) }() // readings for the admin site's server page
	go func() { defer wg.Done(); imports.Run(ctx) }()
	go func() { // cached YouTube data is refreshed or deleted within 30 days, as the privacy policy promises
		defer wg.Done()
		ytStore := youtube.NewStore(pool)
		for {
			if n, err := ytStore.PurgeStale(ctx, 25*24*time.Hour); err != nil {
				slog.Error("purge cached youtube data", "err", err)
			} else if n > 0 {
				slog.Info("purged stale cached youtube data", "connections", n)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(6 * time.Hour):
			}
		}
	}()
	go func() { defer wg.Done(); pusher.Run(ctx) }()
	go func() { defer wg.Done(); emailer.Run(ctx) }()
	go func() { defer wg.Done(); transfers.Run(ctx) }()
	go func() { defer wg.Done(); maint.Run(ctx) }()
	go func() { defer wg.Done(); w.Run(ctx) }()
	go func() { defer wg.Done(); refresher.Run(ctx) }()
	wg.Wait()
	slog.Info("worker stopped")
	return nil
}
