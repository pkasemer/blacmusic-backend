// Package admin holds the endpoints only administrators may call. For now:
// the verification review queue, manual storage grants and the Home playlist groups.
package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/importopt"
	"github.com/blacmusic/backend/internal/playlist"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/review"
	"github.com/blacmusic/backend/internal/showcase"
	"github.com/blacmusic/backend/internal/sysmetrics"
	"github.com/blacmusic/backend/internal/upload"
)

// Handler serves /v1/admin/*.
type Handler struct {
	pool      *pgxpool.Pool
	artists   *artist.Store
	playlists *playlist.Store
	// Showcase is the signed-out showcase the team curates (set by the server).
	Showcase *showcase.Store
	// ImportOptions switches the ways of importing songs on and off (set by the server).
	ImportOptions *importopt.Handler
	// Phase 1 of the admin site (set by the server).
	Auth      *auth.Service
	Redis     *redis.Client
	PublicURL string
	// Playback answers like GET /v1/playback/{id}, for any track (set by the server).
	Playback http.HandlerFunc
	// Media gives links to listen to songs waiting for review (set by the server).
	Media interface {
		PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	}
	// EditorialEmail is the account that owns the editorial playlists (EDITORIAL_OWNER_EMAIL).
	EditorialEmail string
	Bans           interface{ Invalidate(userID string) }
	Accounts       interface {
		AdminDelete(ctx context.Context, userID string, audit func(context.Context, pgx.Tx) error) error
	}
	Releases interface {
		AdminDelete(ctx context.Context, id string, audit func(context.Context, pgx.Tx) error) error
		AdminDeleteTrack(ctx context.Context, id string, audit func(context.Context, pgx.Tx) error) error
		PurgeArtistUploads(ctx context.Context, artistID string) error
		AdminCreate(ctx context.Context, artistID, adminID string, in release.Input, audit func(ctx context.Context, tx pgx.Tx, id string) error) (string, error)
		AdminPublish(ctx context.Context, id string, publishAt *time.Time, audit func(ctx context.Context, tx pgx.Tx) error) error
		AdminUnpublish(ctx context.Context, id string, audit func(ctx context.Context, tx pgx.Tx) error) error
		AdminCombine(ctx context.Context, adminID string, in release.CombineInput,
			audit func(ctx context.Context, tx pgx.Tx, id, artistID string, from []release.Combined) error) (string, error)
		Decide(ctx context.Context, reviewID string, revision int, adminID string, approve bool, reason string,
			audit func(ctx context.Context, tx pgx.Tx, r *review.Row, title string) error) error
		AdminSplit(ctx context.Context, adminID string, in release.SplitInput,
			audit func(ctx context.Context, tx pgx.Tx, id string, from release.SplitFrom) error) (string, error)
	}
	// Uploads lets administrators upload audio into any release (set by the server).
	Uploads *upload.Handler
	// APIKeys manages the limited keys of the bulk import (set by the server).
	APIKeys interface{ KeyRoutes(r chi.Router) }
	// Blobs is used when an artist is taken away from a user and when a failed song is retried; Transfers for the
	// former (both set by the server).
	Blobs interface {
		Stat(ctx context.Context, key string) (int64, error)
		Remove(ctx context.Context, key string) error
		AbortMultipart(ctx context.Context, key, uploadID string) error
	}
	Transfers interface {
		CancelAllOpen(ctx context.Context, userID string) error
	}
	// ReadHost measures the server for GET /system; nil means the real machine (tests replace it).
	ReadHost func() (sysmetrics.Host, bool)
}

// NewHandler creates the handler.
func NewHandler(pool *pgxpool.Pool, artists *artist.Store, playlists *playlist.Store) *Handler {
	return &Handler{pool: pool, artists: artists, playlists: playlists}
}

// Routes registers the routes on r (already authenticated).
func (h *Handler) Routes(r chi.Router) {
	r.Use(h.requireAdmin)
	h.statsRoutes(r)
	h.systemRoutes(r)
	h.processingRoutes(r)
	h.userRoutes(r)
	h.artistRoutes(r)
	h.auditRoutes(r)
	h.releaseRoutes(r)
	h.featuredRoutes(r)
	h.featuredArtistRoutes(r)
	h.lyricsRoutes(r)
	if h.Playback != nil {
		r.Get("/tracks/{id}/playback", h.Playback)
	}
	h.reviewRoutes(r)
	h.metadataRoutes(r)
	h.catalogRoutes(r)
	h.editorialRoutes(r)
	h.searchRoutes(r)
	r.Post("/artists/{id}/verification", h.decide)
	r.Post("/artists/{id}/storage-grants", h.grant)
	h.groupRoutes(r)
	h.showcaseRoutes(r)
	if h.ImportOptions != nil {
		h.ImportOptions.AdminRoutes(r)
	}
	if h.APIKeys != nil {
		h.APIKeys.KeyRoutes(r)
	}
}

// The role is read from the database on every request, so demoting an admin
// takes effect immediately instead of when their token expires.
func (h *Handler) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, _ := auth.UserID(r.Context())
		var role string
		err := h.pool.QueryRow(r.Context(), `SELECT role FROM users WHERE id = $1`, uid).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && role != "admin") {
			httpx.Error(w, http.StatusForbidden, "forbidden", "administrators only")
			return
		}
		if err != nil {
			slog.Error("admin role lookup", "err", err)
			httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// POST /v1/admin/artists/{id}/verification {decision: approve|reject|revoke|verify, note}
//
// approve and reject answer a pending request; revoke takes the badge away; verify gives it to any artist who does
// not have it, whether or not they asked.
func (h *Handler) decide(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	err := httpx.Decode(w, r, &req)
	switch req.Decision {
	case "approve", "reject", "revoke", "verify":
	default:
		err = errors.New("unknown decision")
	}
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "decision must be approve, reject, revoke or verify")
		return
	}
	note := strings.TrimSpace(req.Note)
	if (req.Decision == "reject" || req.Decision == "revoke") && note == "" {
		reply(w, invalid("a rejection or revoke needs a note of 1-500 characters"))
		return
	}
	if utf8.RuneCountInString(note) > 500 {
		reply(w, invalid("a note is at most 500 characters"))
		return
	}
	// The row change and the audit row commit together.
	from, to, action := "pending", "verified", "artist.verify"
	switch req.Decision {
	case "reject":
		to, action = "rejected", "artist.reject"
	case "revoke":
		from, to, action = "verified", "unverified", "artist.revoke"
	}
	// The audit log always keeps the note. The artist is only shown it when it explains a "no".
	shown := note
	if to == "verified" {
		shown = ""
	}
	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	id := chi.URLParam(r, "id")
	var name, state string
	var banned bool
	if err := tx.QueryRow(ctx, `
		SELECT a.name, a.verification, a.banned_at IS NOT NULL OR u.banned_at IS NOT NULL
		FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id WHERE a.id = $1 FOR UPDATE OF a`, id).Scan(&name, &state, &banned); err != nil {
		reply(w, err)
		return
	}
	if req.Decision == "verify" {
		switch {
		case banned:
			reply(w, fail4(http.StatusConflict, "artist_banned", "this artist is banned"))
			return
		case state == "verified":
			reply(w, fail4(http.StatusConflict, "already_verified", "this artist is already verified"))
			return
		}
		from = state
	}
	if state != from {
		if from == "pending" {
			reply(w, fail4(http.StatusConflict, "not_pending", "there is no pending verification request"))
		} else {
			reply(w, fail4(http.StatusConflict, "not_verified", "this artist is not verified"))
		}
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE artists SET verification = $2, verification_note = $3, updated_at = now(),
		flagged = CASE WHEN $2 = 'verified' THEN false ELSE flagged END WHERE id = $1`, id, to, shown); err != nil {
		reply(w, err)
		return
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: action, TargetType: "artist", TargetID: id, TargetLabel: name, Reason: note,
		Details: map[string]any{"from": from, "to": to}}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/admin/artists/{id}/storage-grants {kind, bytes, note, reference, expiresAt}
// A repeated reference changes nothing (200), so payment webhooks can retry safely.
func (h *Handler) grant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind      string     `json:"kind"`
		Bytes     int64      `json:"bytes"`
		Note      string     `json:"note"`
		Reference string     `json:"reference"`
		ExpiresAt *time.Time `json:"expiresAt"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	created, err := h.artists.Grant(r.Context(), chi.URLParam(r, "id"), req.Kind, req.Bytes, req.Note, req.Reference, req.ExpiresAt)
	if err != nil {
		artist.Fail(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		_ = audit.Write(r.Context(), h.pool, audit.Entry{ActorID: actorID(r), Action: "artist.grant", TargetType: "artist", TargetID: chi.URLParam(r, "id"),
			Reason: req.Note, Details: map[string]any{"kind": req.Kind, "bytes": req.Bytes, "reference": req.Reference}, IP: audit.IP(r)})
	}
	httpx.JSON(w, status, map[string]any{"created": created})
}
