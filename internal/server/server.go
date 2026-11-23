// Package server wires routes and middleware.
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/catalog"
	"github.com/blacmusic/backend/internal/config"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/library"
	"github.com/blacmusic/backend/internal/playback"
	"github.com/blacmusic/backend/internal/playlist"
	"github.com/blacmusic/backend/internal/ratelimit"
	"github.com/blacmusic/backend/internal/source"
	"github.com/blacmusic/backend/internal/storage"
	"github.com/blacmusic/backend/internal/upload"
)

// Deps are the long-lived dependencies the HTTP layer needs.
type Deps struct {
	Cfg      config.Config
	Pool     *pgxpool.Pool
	Redis    *redis.Client
	Store    *storage.Store
	Registry *source.Registry
}

// Handler builds the full HTTP router.
func Handler(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.Recoverer, chimw.Timeout(30*time.Second))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
		defer cancel()
		checks := map[string]string{"postgres": "ok", "redis": "ok", "storage": "ok"}
		status := http.StatusOK
		if err := d.Pool.Ping(ctx); err != nil {
			checks["postgres"], status = "down", http.StatusServiceUnavailable
		}
		if err := d.Redis.Ping(ctx).Err(); err != nil {
			checks["redis"], status = "down", http.StatusServiceUnavailable
		}
		if err := d.Store.Ping(ctx); err != nil {
			checks["storage"], status = "down", http.StatusServiceUnavailable
		}
		httpx.JSON(w, status, checks)
	})

	tokens := auth.NewTokenService(d.Cfg.JWTSecret, d.Cfg.AccessTTL)
	authSvc := auth.NewService(d.Pool, tokens, d.Cfg.RefreshTTL)
	authH := auth.NewHandler(authSvc)
	catalogH := catalog.NewHandler(d.Pool, d.Registry)
	playbackH := playback.NewHandler(d.Pool, d.Registry)
	libraryH := library.NewHandler(d.Pool)
	playlistH := playlist.NewHandler(playlist.NewStore(d.Pool))
	uploadH := upload.NewHandler(d.Pool, d.Store, d.Cfg.UploadMaxBytes, d.Cfg.UploadURLTTL)

	r.Route("/v1", func(v1 chi.Router) {
		v1.Group(func(pub chi.Router) {
			pub.Use(ratelimit.Middleware(d.Redis, "auth", d.Cfg.AuthRateLimit, d.Cfg.AuthRateWindow))
			pub.Post("/auth/register", authH.Register)
			pub.Post("/auth/login", authH.Login)
			pub.Post("/auth/refresh", authH.Refresh)
			pub.Post("/auth/logout", authH.Logout)
		})

		v1.Group(func(priv chi.Router) {
			priv.Use(auth.Require(tokens))
			priv.Get("/me", authH.Me)
			priv.Get("/search", catalogH.Search)
			priv.Get("/tracks/{id}", catalogH.GetTrack)
			priv.Get("/playback/{id}", playbackH.Resolve)

			priv.Route("/me/library", libraryH.Routes)
			priv.Route("/uploads", uploadH.Routes)

			priv.Get("/me/playlists", playlistH.ListMine)
			priv.Post("/playlists", playlistH.Create)
			priv.Get("/playlists/{id}", playlistH.Get)
			priv.Patch("/playlists/{id}", playlistH.Update)
			priv.Delete("/playlists/{id}", playlistH.Delete)
			priv.Post("/playlists/{id}/items", playlistH.AddItem)
			priv.Delete("/playlists/{id}/items/{itemId}", playlistH.RemoveItem)
			priv.Put("/playlists/{id}/items/{itemId}/position", playlistH.MoveItem)
		})
	})
	return r
}
