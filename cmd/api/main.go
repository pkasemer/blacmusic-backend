// Command api runs the Blac Music HTTP API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/blacmusic/backend/internal/config"
	"github.com/blacmusic/backend/internal/db"
	"github.com/blacmusic/backend/internal/mailer"
	"github.com/blacmusic/backend/internal/server"
	"github.com/blacmusic/backend/internal/source"
	"github.com/blacmusic/backend/internal/source/native"
	podcastsrc "github.com/blacmusic/backend/internal/source/podcast"
	"github.com/blacmusic/backend/internal/storage"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
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
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return err
	}
	rdb := redis.NewClient(redisOpts)
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return err
	}

	store, err := storage.New(cfg)
	if err != nil {
		return err
	}
	if err := store.EnsureBucket(ctx); err != nil {
		return err
	}

	// Register audio sources. New providers (podcast, audius, jamendo, a
	// client-resolved youtube reference provider...) are added here.
	reg := source.NewRegistry()
	reg.Register(native.New(pool, store, cfg.PlaybackURLTTL, cfg.PublicAPIURL))
	reg.Register(podcastsrc.New(pool))

	mail, err := mailer.FromConfig(&cfg)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           server.Handler(server.Deps{Cfg: cfg, Pool: pool, Redis: rdb, Store: store, Registry: reg, Mailer: mail}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.HTTPAddr, "env", cfg.Env)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
