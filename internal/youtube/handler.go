package youtube

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/titles"
)

// maxVideos bounds what one sync reads (about 10 API units).
const maxVideos = 500

// Handler serves /v1/integrations/youtube. provider may be nil when the
// feature is not configured: every route then answers 503 youtube_unavailable.
type Handler struct {
	store       *Store
	provider    Provider
	vault       *Vault
	appRedirect string
	// OnDisconnect runs after a user disconnects (the transfer queue cancels their open transfers).
	OnDisconnect func(ctx context.Context, userID string) error
	// AudioImport tells the app whether the server can fetch the audio of an artist's own videos.
	AudioImport bool
}

// NewHandler creates the handler.
func NewHandler(store *Store, provider Provider, vault *Vault, appRedirect string) *Handler {
	return &Handler{store: store, provider: provider, vault: vault, appRedirect: appRedirect}
}

// Routes registers the authenticated routes.
func (h *Handler) Routes(r chi.Router) {
	r.Use(h.requireProvider)
	r.Post("/connect", h.connect)
	r.Get("/", h.status)
	r.Delete("/", h.disconnect)
	r.Post("/sync", h.sync)
	r.Get("/videos", h.videos)
}

func (h *Handler) requireProvider(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.provider == nil || h.vault == nil {
			httpx.Error(w, http.StatusServiceUnavailable, "youtube_unavailable", "YouTube connect is not available right now")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// POST /v1/integrations/youtube/connect -> {authUrl, expiresInSeconds}
func (h *Handler) connect(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	state, err1 := randomToken()
	verifier, err2 := randomToken()
	if err1 != nil || err2 != nil {
		serverError(w, errors.Join(err1, err2))
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if err := h.store.SaveState(r.Context(), uid, state, verifier); err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"authUrl": h.provider.AuthURL(state, challenge), "expiresInSeconds": int(stateTTL.Seconds()),
	})
}

// Callback is where Google sends the browser back (public: the user's browser,
// not the app, calls it). It always ends in a redirect to the app.
func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	if h.provider == nil || h.vault == nil {
		h.redirectApp(w, r, "error", "unavailable")
		return
	}
	q := r.URL.Query()
	if q.Get("error") != "" {
		reason := "access_denied"
		if q.Get("error") != "access_denied" {
			reason = "google_error"
		}
		// consume the state so it cannot be replayed
		_, _, _ = h.store.TakeState(r.Context(), q.Get("state"))
		h.redirectApp(w, r, "error", reason)
		return
	}
	uid, verifier, err := h.store.TakeState(r.Context(), q.Get("state"))
	if err != nil || q.Get("code") == "" {
		h.redirectApp(w, r, "error", "state_expired")
		return
	}
	tok, err := h.provider.Exchange(r.Context(), q.Get("code"), verifier)
	if err != nil {
		slog.Warn("youtube code exchange", "err", err)
		h.redirectApp(w, r, "error", "exchange_failed")
		return
	}
	if tok.RefreshToken == "" || (tok.Scope != "" && !strings.Contains(tok.Scope, ReadOnlyScope)) {
		h.redirectApp(w, r, "error", "missing_permission")
		return
	}
	ch, err := h.provider.Channel(r.Context(), tok.AccessToken)
	if errors.Is(err, ErrNoChannel) {
		h.redirectApp(w, r, "error", "no_channel")
		return
	}
	if err != nil {
		slog.Warn("youtube channel lookup", "err", err)
		h.redirectApp(w, r, "error", "google_error")
		return
	}
	sealed, err := h.vault.Seal(tok.RefreshToken, uid)
	if err != nil {
		slog.Error("youtube seal", "err", err)
		h.redirectApp(w, r, "error", "server_error")
		return
	}
	if err := h.store.Upsert(r.Context(), uid, ch, sealed, ReadOnlyScope); err != nil {
		slog.Error("youtube save connection", "err", err)
		h.redirectApp(w, r, "error", "server_error")
		return
	}
	h.redirectApp(w, r, "ok", "")
}

func (h *Handler) redirectApp(w http.ResponseWriter, r *http.Request, status, reason string) {
	q := url.Values{"status": {status}}
	if reason != "" {
		q.Set("reason", reason)
	}
	sep := "?"
	if strings.Contains(h.appRedirect, "?") {
		sep = "&"
	}
	http.Redirect(w, r, h.appRedirect+sep+q.Encode(), http.StatusFound)
}

type statusView struct {
	Connected      bool       `json:"connected"`
	Channel        *chanView  `json:"channel,omitempty"`
	ConnectedAt    *time.Time `json:"connectedAt,omitempty"`
	LastSyncedAt   *time.Time `json:"lastSyncedAt,omitempty"`
	NeedsReconnect bool       `json:"needsReconnect"`
	// AudioImport: the server can import the audio of the songs you pick (otherwise you upload files).
	AudioImport bool `json:"audioImport"`
}

type chanView struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	ThumbnailURL string `json:"thumbnailUrl,omitempty"`
}

// GET /v1/integrations/youtube
func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	c, err := h.store.Get(r.Context(), uid)
	if errors.Is(err, ErrNotConnected) {
		httpx.JSON(w, http.StatusOK, statusView{AudioImport: h.AudioImport})
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, statusView{
		Connected: true, Channel: &chanView{ID: c.ChannelID, Title: c.ChannelTitle, ThumbnailURL: c.ThumbnailURL},
		ConnectedAt: &c.ConnectedAt, LastSyncedAt: c.LastSyncedAt, NeedsReconnect: c.Status == "needs_reconnect", AudioImport: h.AudioImport,
	})
}

// DELETE /v1/integrations/youtube: revoke at Google, then forget everything.
func (h *Handler) disconnect(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	if err := h.Forget(r.Context(), uid); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Forget revokes the user's connection at Google (best effort), deletes the
// stored tokens and cached videos, and runs OnDisconnect. Nothing connected is not an error.
func (h *Handler) Forget(ctx context.Context, uid string) error {
	c, err := h.store.Get(ctx, uid)
	if errors.Is(err, ErrNotConnected) {
		return nil
	}
	if err != nil {
		return err
	}
	if h.provider != nil && h.vault != nil {
		if token, oerr := h.vault.Open(c.RefreshEnc, uid); oerr == nil {
			if rerr := h.provider.Revoke(ctx, token); rerr != nil {
				slog.Warn("youtube revoke failed (tokens are deleted anyway)", "err", rerr)
			}
		}
	}
	if err := h.store.Delete(ctx, uid); err != nil {
		return err
	}
	if h.OnDisconnect != nil {
		if err := h.OnDisconnect(ctx, uid); err != nil {
			slog.Error("youtube disconnect hook", "err", err)
		}
	}
	return nil
}

// accessToken refreshes the user's access token. ErrInvalidGrant also marks the
// connection as needing a reconnect.
func (h *Handler) accessToken(ctx context.Context, uid string) (string, error) {
	c, err := h.store.Get(ctx, uid)
	if err != nil {
		return "", err
	}
	if c.Status == "needs_reconnect" {
		return "", ErrInvalidGrant
	}
	refresh, err := h.vault.Open(c.RefreshEnc, uid)
	if err != nil {
		return "", err
	}
	tok, err := h.provider.Refresh(ctx, refresh)
	if errors.Is(err, ErrInvalidGrant) {
		_ = h.store.SetStatus(ctx, uid, "needs_reconnect")
	}
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

func (h *Handler) doSync(ctx context.Context, uid string) (int, error) {
	access, err := h.accessToken(ctx, uid)
	if err != nil {
		return 0, err
	}
	videos, err := h.provider.Videos(ctx, access, maxVideos)
	if errors.Is(err, ErrInvalidGrant) {
		_ = h.store.SetStatus(ctx, uid, "needs_reconnect")
	}
	if err != nil {
		return 0, err
	}
	if err := h.store.ReplaceVideos(ctx, uid, videos); err != nil {
		return 0, err
	}
	return len(videos), nil
}

// POST /v1/integrations/youtube/sync: re-read the channel's uploads now.
func (h *Handler) sync(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	n, err := h.doSync(r.Context(), uid)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"videoCount": n, "syncedAt": time.Now().UTC()})
}

// GET /v1/integrations/youtube/videos?limit=&offset=&music=1&refresh=1
func (h *Handler) videos(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	q := r.URL.Query()
	c, err := h.store.Get(r.Context(), uid)
	if err != nil {
		h.fail(w, err)
		return
	}
	if q.Get("refresh") == "1" || c.LastSyncedAt == nil {
		if _, err := h.doSync(r.Context(), uid); err != nil {
			h.fail(w, err)
			return
		}
		c, _ = h.store.Get(r.Context(), uid)
	}
	vs, err := h.store.Videos(r.Context(), uid)
	if err != nil {
		serverError(w, err)
		return
	}
	owned, err := h.store.OwnedTitles(r.Context(), uid, titles.Norm)
	if err != nil {
		serverError(w, err)
		return
	}
	views := BuildViews(vs, c.ChannelTitle, owned)
	if q.Get("music") != "0" {
		kept := views[:0]
		for _, v := range views {
			if v.Kind == "song" {
				kept = append(kept, v)
			}
		}
		views = kept
	}
	limit, offset := 50, 0
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, 200)
	}
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n > 0 {
		offset = min(n, len(views))
	}
	total := len(views)
	views = views[offset:min(offset+limit, len(views))]
	if views == nil {
		views = []VideoView{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"videos": views, "total": total, "syncedAt": c.LastSyncedAt})
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	var api *APIError
	switch {
	case errors.Is(err, ErrNotConnected):
		httpx.Error(w, http.StatusConflict, "youtube_not_connected", "connect your YouTube channel first")
	case errors.Is(err, ErrInvalidGrant):
		httpx.Error(w, http.StatusConflict, "reconnect_required", "YouTube access was revoked; connect again")
	case errors.Is(err, ErrQuota):
		w.Header().Set("Retry-After", "3600")
		httpx.Error(w, http.StatusServiceUnavailable, "youtube_quota", "YouTube is busy right now, try again later")
	case errors.As(err, &api):
		slog.Warn("youtube api error", "status", api.Status)
		httpx.Error(w, http.StatusBadGateway, "youtube_error", "YouTube did not answer as expected, try again")
	default:
		serverError(w, err)
	}
}

func serverError(w http.ResponseWriter, err error) {
	slog.Error("youtube handler", "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
}
