// Package server wires routes and middleware.
package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/blacmusic/backend/internal/account"
	"github.com/blacmusic/backend/internal/admin"
	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/catalog"
	"github.com/blacmusic/backend/internal/config"
	"github.com/blacmusic/backend/internal/credits"
	"github.com/blacmusic/backend/internal/genre"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/identity"
	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/importer"
	"github.com/blacmusic/backend/internal/importopt"
	"github.com/blacmusic/backend/internal/legal"
	"github.com/blacmusic/backend/internal/library"
	"github.com/blacmusic/backend/internal/lyrics"
	"github.com/blacmusic/backend/internal/mailer"
	"github.com/blacmusic/backend/internal/notify"
	"github.com/blacmusic/backend/internal/playback"
	"github.com/blacmusic/backend/internal/playlist"
	"github.com/blacmusic/backend/internal/podcast"
	"github.com/blacmusic/backend/internal/profile"
	"github.com/blacmusic/backend/internal/push"
	"github.com/blacmusic/backend/internal/ratelimit"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/searchlog"
	"github.com/blacmusic/backend/internal/showcase"
	"github.com/blacmusic/backend/internal/source"
	"github.com/blacmusic/backend/internal/stats"
	"github.com/blacmusic/backend/internal/storage"
	"github.com/blacmusic/backend/internal/transfer"
	"github.com/blacmusic/backend/internal/upload"
	"github.com/blacmusic/backend/internal/youtube"
)

// Deps are the long-lived dependencies the HTTP layer needs.
type Deps struct {
	Cfg      config.Config
	Pool     *pgxpool.Pool
	Redis    *redis.Client
	Store    *storage.Store
	Registry *source.Registry
	Mailer   mailer.Mailer
}

// Handler builds the full HTTP router.
func Handler(d Deps) http.Handler {
	r := chi.NewRouter()
	if d.Cfg.TrustProxy {
		r.Use(chimw.RealIP) // so rate limits see the visitor, not the proxy
	}
	if d.Cfg.S3PublicHostFromRequest {
		r.Use(publicHostFromRequest)
	}
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
	authSvc.SetMailer(d.Mailer)
	authSvc.SetSocial(socialConfig(d.Cfg))
	authSvc.SetAvatarURL(func(id string) string { return image.URL(d.Cfg.PublicAPIURL, id, image.SizeMedium) })
	authH := auth.NewHandler(authSvc)
	catalogH := catalog.NewHandler(d.Pool, d.Registry, d.Cfg.PublicAPIURL)
	playbackH := playback.NewHandler(d.Pool, d.Registry)
	playlistStore := playlist.NewStore(d.Pool, d.Cfg.PublicAPIURL)
	libraryH := library.NewHandler(d.Pool, playlistStore)
	libraryH.PublicURL = d.Cfg.PublicAPIURL
	playlistH := playlist.NewHandler(playlistStore)
	podcastH := podcast.NewHandler(podcast.NewStore(d.Pool,
		podcast.NewFetcher(d.Cfg.FeedAllowPrivate, d.Cfg.FeedFetchTimeout), d.Cfg.FeedRefreshInterval))
	uploadStore := upload.NewStore(d.Pool)
	uploadH := upload.NewHandler(uploadStore, d.Store, upload.Options{
		MaxBytes: d.Cfg.UploadMaxBytes, URLTTL: d.Cfg.UploadURLTTL, PartSize: d.Cfg.UploadPartBytes,
	})
	artistStore := artist.NewStore(d.Pool, d.Cfg.FreeStorageBytes, d.Cfg.PublicAPIURL)
	artistH := artist.NewHandler(artistStore)
	releaseStore := release.NewStore(d.Pool, d.Store, d.Cfg.PublicAPIURL)
	releaseStore.OnPublish = notify.OnReleasePublished // the confirmation email is queued with the publish
	releaseH := release.NewHandler(releaseStore)
	uploadH.Tracks = releaseStore
	imageH := image.NewHandler(d.Pool, d.Store, d.Cfg.PublicAPIURL)
	pushH := push.NewHandler(push.NewStore(d.Pool))
	showcaseStore := showcase.NewStore(d.Pool, d.Cfg.PublicAPIURL)
	adminH := admin.NewHandler(d.Pool, artistStore, playlistStore)
	adminH.Showcase = showcaseStore
	profileH := profile.NewHandler(d.Pool, authSvc)

	ytStore := youtube.NewStore(d.Pool)
	ytProvider, ytVault := youtubeProvider(d.Cfg)
	ytH := youtube.NewHandler(ytStore, ytProvider, ytVault, d.Cfg.YouTubeAppRedirect)
	transfers := transfer.NewManager(d.Pool, ytStore, releaseStore, uploadStore)
	transfers.FetchEnabled = d.Cfg.YoutubeAudioFetch
	transfers.Blobs = d.Store
	lyricsH := lyrics.NewHandler(lyrics.NewStore(d.Pool))
	creditsH := credits.NewHandler(credits.NewStore(d.Pool))
	statsH := stats.NewHandler(stats.NewStore(d.Pool))
	importStore := importopt.NewStore(d.Pool)
	transfers.Options = importStore
	importH := importopt.NewHandler(importStore)
	adminH.ImportOptions = importH
	ytH.OnDisconnect = transfers.CancelAllOpen
	ytH.AudioImport = d.Cfg.YoutubeAudioFetch
	transferH := transfer.NewHandler(transfers)
	accounts := account.NewService(d.Pool, releaseStore, d.Store)
	accounts.Before = append(accounts.Before, ytH.Forget)
	accountH := account.NewHandler(accounts)
	searchLog := searchlog.New(context.Background(), d.Pool)
	bans := auth.NewBanCache(d.Pool, 30*time.Second) // banned users are refused on every authenticated request
	adminH.Auth, adminH.Redis, adminH.PublicURL = authSvc, d.Redis, d.Cfg.PublicAPIURL
	adminH.EditorialEmail = d.Cfg.EditorialOwnerEmail
	adminH.Uploads = uploadH
	uploadH.OnAdminComplete = adminH.OnUploadComplete
	adminH.Bans, adminH.Accounts, adminH.Releases = bans, accounts, releaseStore
	// bulk catalogue import: its own routes also take a limited import key; the keys are managed under /v1/admin
	bulkH := importer.NewHandler(d.Pool, d.Store, tokens, importer.Options{
		MaxAudioBytes: d.Cfg.UploadMaxBytes, PartSize: d.Cfg.UploadPartBytes, URLTTL: d.Cfg.UploadURLTTL,
		MaxTotalBytes: d.Cfg.ImportMaxTotalBytes, MinFreeBytes: d.Cfg.ImportMinFreeBytes, AdminSiteURL: d.Cfg.AdminSiteURL,
	})
	adminH.APIKeys = bulkH

	// Public pages that Google and the app stores link to.
	if lp, err := legal.New(d.Cfg.LegalOperator, d.Cfg.PublicAPIURL, d.Cfg.LegalEmail, d.Cfg.YoutubeAudioFetch); err != nil {
		slog.Error("legal pages disabled", "err", err)
	} else {
		r.Get("/", lp.Home)
		r.Get("/privacy", lp.Privacy)
		r.Get("/terms", lp.Terms)
	}

	r.Route("/v1", func(v1 chi.Router) {
		// Avatars and cover art are public: apps load them with plain image
		// requests. Ids are unguessable and the files are re-encoded JPEGs.
		v1.Route("/media", imageH.ServeRoute)

		// Authenticated inside: an administrator's token or an import key, which opens nothing else.
		v1.Route("/admin/imports", bulkH.Routes)

		// The signed-out screens load the curated showcase before anyone has an account.
		v1.With(ratelimit.Middleware(d.Redis, "showcase", 60, time.Minute)).Get("/showcase", showcase.NewHandler(showcaseStore).List)

		// Google sends the user's browser here after consent, so it carries no bearer token.
		v1.Get("/integrations/youtube/callback", ytH.Callback)

		v1.Group(func(pub chi.Router) {
			pub.Use(ratelimit.Middleware(d.Redis, "auth", d.Cfg.AuthRateLimit, d.Cfg.AuthRateWindow))
			pub.Post("/auth/register", authH.Register)
			pub.Post("/auth/login", authH.Login)
			pub.Post("/auth/refresh", authH.Refresh)
			pub.Post("/auth/logout", authH.Logout)
			pub.Post("/auth/apple", authH.Apple)
			pub.Post("/auth/google/start", authH.GoogleStart)
			pub.Get("/auth/google/callback", authH.GoogleCallback)
			pub.Post("/auth/google/exchange", authH.GoogleExchange)
			pub.Post("/admin/auth/login", adminH.Login) // the admin site's sign-in: outside the authenticated group
		})

		v1.Group(func(priv chi.Router) {
			priv.Use(auth.RequireWith(tokens, bans))
			priv.Use(searchLog.Middleware) // records what is searched (first page only, no user, no address)
			priv.Get("/me", authH.Me)
			priv.Patch("/me", profileH.Update)
			priv.Delete("/me", accountH.Delete)
			pushH.Routes(priv)
			if d.Cfg.PushFake {
				pushH.DevRoutes(priv) // development only: what would have been pushed
			}
			priv.Post("/auth/email/send-code", authH.SendEmailCode)
			priv.Post("/auth/email/verify", authH.VerifyEmail)
			priv.Get("/search", catalogH.Search)
			priv.Get("/browse/tracks", catalogH.Browse)
			priv.Get("/browse/artists", catalogH.BrowseArtists)
			priv.Get("/browse/playlists", playlistH.Browse)
			priv.Get("/browse/playlist-groups", playlistH.BrowseGroups)
			priv.Get("/browse/playlist-groups/{id}", playlistH.GetGroup)
			priv.Get("/search/playlists", playlistH.Search)
			priv.Get("/search/artists", catalogH.SearchArtists)
			priv.Get("/artists/{id}/tracks", catalogH.ArtistTracks)
			priv.Get("/artists/{id}/related", catalogH.RelatedArtists)
			priv.Get("/genres", genre.List)
			priv.Get("/tracks/{id}", catalogH.GetTrack)
			priv.Get("/playback/{id}", playbackH.Resolve)

			priv.Route("/me/library", libraryH.Routes)
			priv.Get("/import-options", importH.List)
			statsH.Routes(priv, ratelimit.Middleware(d.Redis, "plays", 5000, time.Hour))
			lyricsH.Routes(priv)
			creditsH.Routes(priv)
			priv.Route("/uploads", uploadH.Routes)
			uploadH.TrackRoutes(priv)
			priv.Route("/images", imageH.Routes)
			priv.Route("/admin", adminH.Routes)
			priv.Route("/integrations/youtube", ytH.Routes)
			priv.Route("/transfers", transferH.Routes)
			artistH.Routes(priv)
			releaseH.Routes(priv)

			// Adding a show makes the server fetch a user-supplied URL, so it is rate limited.
			podcastH.Routes(priv, ratelimit.Middleware(d.Redis, "show-add", 30, time.Hour))

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

// socialConfig picks the sign-in providers. Apple needs only our app ids (no secret);
// Google needs an OAuth client, or the built-in fake in development.
func socialConfig(cfg config.Config) auth.SocialConfig {
	sc := auth.SocialConfig{AppRedirect: cfg.GoogleAuthAppRedirect}
	if len(cfg.AppleBundleIDs) > 0 || cfg.AppleAuthFake {
		a := identity.NewApple(cfg.AppleBundleIDs)
		a.Fake = cfg.AppleAuthFake
		sc.Apple = a
	}
	switch {
	case cfg.GoogleAuthFake:
		sc.Google = &identity.FakeGoogle{CallbackURL: cfg.GoogleAuthRedirectURL}
	case cfg.GoogleAuthClientID != "":
		sc.Google = identity.NewGoogleOAuth(cfg.GoogleAuthClientID, cfg.GoogleAuthSecret, cfg.GoogleAuthRedirectURL)
	}
	return sc
}

// youtubeProvider picks the YouTube backend: the real Google one when an OAuth
// client is configured, the built-in fake in development when asked, otherwise
// none (the feature answers 503).
func youtubeProvider(cfg config.Config) (youtube.Provider, *youtube.Vault) {
	var p youtube.Provider
	switch {
	case cfg.YouTubeFake:
		p = youtube.NewFake(cfg.YouTubeRedirectURL)
	case cfg.GoogleClientID != "":
		p = youtube.NewGoogle(cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.YouTubeRedirectURL)
	default:
		return nil, nil
	}
	key := cfg.TokenEncKey
	if key == nil {
		key = youtube.DevKey(cfg.JWTSecret) // production requires TOKEN_ENC_KEY (checked in config)
	}
	v, err := youtube.NewVault(key)
	if err != nil {
		slog.Error("youtube disabled", "err", err)
		return nil, nil
	}
	return p, v
}

// publicHostFromRequest (development only) makes presigned storage links name
// the host the client used to reach the API, e.g. the PC's LAN address.
func publicHostFromRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		next.ServeHTTP(w, r.WithContext(storage.WithPublicHost(r.Context(), host)))
	})
}
