// Package onboarding serves the app's first-run questions: the artists to pick from before there is an account, and
// the listener's choices afterwards.
package onboarding

import (
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/image"
)

// Handler serves /v1/onboarding.
type Handler struct {
	pool      *pgxpool.Pool
	publicURL string
}

// NewHandler creates the handler.
func NewHandler(pool *pgxpool.Pool, publicURL string) *Handler {
	return &Handler{pool: pool, publicURL: publicURL}
}

// catalogue is the languages, regions and genres the app asks about. Their ids are stored in listeners' choices:
// never change one.
//
//go:embed catalogue.json
var catalogue []byte

// Config is GET /v1/onboarding/config.
func (h *Handler) Config(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	httpx.JSON(w, http.StatusOK, json.RawMessage(catalogue))
}

type artistView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ImageURL string `json:"imageUrl,omitempty"`
	Verified bool   `json:"verified"`
}

// Artists is GET /v1/onboarding/artists?q=&limit=, open to anyone: artists who are not banned and have music online.
// Without q the most played of the last 30 days (by listeners) come first, then the most followed.
func (h *Handler) Artists(w http.ResponseWriter, r *http.Request) {
	limit := 30
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, 50)
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if utf8.RuneCountInString(q) > 100 {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "q is at most 100 characters")
		return
	}
	pattern := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	rows, err := h.pool.Query(r.Context(), `
		SELECT a.id, a.name, COALESCE(u.avatar_image_id, a.avatar_image_id), COALESCE(a.image_url, ''), a.verification = 'verified'
		FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id
		WHERE a.banned_at IS NULL AND ($1 = '' OR a.name ILIKE '%' || $1 || '%')
		  AND EXISTS (SELECT 1 FROM albums al JOIN tracks t ON t.album_id = al.id
		              WHERE al.artist_id = a.id AND al.visibility = 'public' AND t.visibility = 'public')
		ORDER BY ($1 <> '' AND a.name ILIKE $1 || '%') DESC,
		  (SELECT count(DISTINCT p.user_id) FROM signal_plays p WHERE p.artist_id = a.id AND p.at > now() - interval '30 days') DESC,
		  (SELECT count(*) FROM signal_follows l WHERE l.artist_id = a.id) DESC, lower(a.name), a.id
		LIMIT $2`, pattern, limit)
	if err != nil {
		internal(w, "onboarding artists", err)
		return
	}
	defer rows.Close()
	out := []artistView{}
	for rows.Next() {
		var a artistView
		var avatarID *string
		var legacy string
		if err := rows.Scan(&a.ID, &a.Name, &avatarID, &legacy, &a.Verified); err != nil {
			internal(w, "onboarding artists", err)
			return
		}
		a.ImageURL = image.PersonURL(h.publicURL, avatarID, legacy)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		internal(w, "onboarding artists", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"artists": out})
}

type languages struct {
	Preferred  []string `json:"preferred"`
	Understood []string `json:"understood"`
}

// Preferences are the listener's choices.
type Preferences struct {
	Languages     languages `json:"languages"`
	Regions       []string  `json:"regions"`
	DiscoveryMode string    `json:"discoveryMode,omitempty"`
	Genres        []string  `json:"genres"`
	ArtistIDs     []string  `json:"artistIds"`
}

var discoveryModes = map[string]bool{"": true, "familiar": true, "explore_more": true, "everything_uganda": true}

// clean trims a list of ids and drops repeats, keeping the order.
func clean(in []string, most int) ([]string, bool) {
	out, seen := []string{}, map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || utf8.RuneCountInString(s) > 64 {
			return nil, false
		}
		if !seen[s] {
			seen[s], out = true, append(out, s)
		}
	}
	return out, len(out) <= most
}

// Save is POST /v1/onboarding/preferences: it replaces all the listener's choices.
func (h *Handler) Save(w http.ResponseWriter, r *http.Request) {
	var in Preferences
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	for _, l := range []struct {
		name string
		list *[]string
		most int
	}{
		{"languages.preferred", &in.Languages.Preferred, 50}, {"languages.understood", &in.Languages.Understood, 50},
		{"regions", &in.Regions, 50}, {"genres", &in.Genres, 50}, {"artistIds", &in.ArtistIDs, 100},
	} {
		out, ok := clean(*l.list, l.most)
		if !ok {
			httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", l.name+" has too many entries, or one that is empty or over 64 characters")
			return
		}
		*l.list = out
	}
	if !discoveryModes[in.DiscoveryMode] {
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", "discoveryMode must be familiar, explore_more or everything_uganda")
		return
	}
	uid, _ := auth.UserID(r.Context())
	// artists that do not exist are left out
	if _, err := h.pool.Exec(r.Context(), `
		INSERT INTO listener_preferences (user_id, preferred_languages, understood_languages, regions, discovery_mode, genres, artist_ids)
		VALUES ($1, $2, $3, $4, $5, $6,
		        ARRAY(SELECT n.id FROM unnest($7::text[]) WITH ORDINALITY AS n(id, i) JOIN artists a ON a.id = n.id ORDER BY n.i))
		ON CONFLICT (user_id) DO UPDATE SET preferred_languages = EXCLUDED.preferred_languages,
		  understood_languages = EXCLUDED.understood_languages, regions = EXCLUDED.regions, discovery_mode = EXCLUDED.discovery_mode,
		  genres = EXCLUDED.genres, artist_ids = EXCLUDED.artist_ids, updated_at = now()`,
		uid, in.Languages.Preferred, in.Languages.Understood, in.Regions, in.DiscoveryMode, in.Genres, in.ArtistIDs); err != nil {
		internal(w, "save preferences", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Get is GET /v1/onboarding/preferences.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	out := struct {
		Preferences
		Completed bool `json:"completed"`
	}{Preferences: Preferences{Languages: languages{[]string{}, []string{}}, Regions: []string{}, Genres: []string{}, ArtistIDs: []string{}}}
	err := h.pool.QueryRow(r.Context(), `
		SELECT preferred_languages, understood_languages, regions, discovery_mode, genres, artist_ids, completed_at IS NOT NULL
		FROM listener_preferences WHERE user_id = $1`, uid).
		Scan(&out.Languages.Preferred, &out.Languages.Understood, &out.Regions, &out.DiscoveryMode, &out.Genres, &out.ArtistIDs, &out.Completed)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		internal(w, "read preferences", err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Complete is POST /v1/onboarding/complete: onboarding is done, with or without choices, and the listener's
// recommendation profile starts. Repeating it changes nothing.
func (h *Handler) Complete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid, _ := auth.UserID(ctx)
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		internal(w, "complete onboarding", err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, q := range []string{
		`INSERT INTO listener_preferences (user_id, completed_at) VALUES ($1, now())
		 ON CONFLICT (user_id) DO UPDATE SET completed_at = COALESCE(listener_preferences.completed_at, now())`,
		`INSERT INTO recommendation_profiles (user_id, seed)
		 SELECT user_id, jsonb_build_object('languages', jsonb_build_object('preferred', preferred_languages, 'understood', understood_languages),
		        'regions', regions, 'discoveryMode', discovery_mode, 'genres', genres, 'artistIds', artist_ids)
		 FROM listener_preferences WHERE user_id = $1 ON CONFLICT (user_id) DO NOTHING`,
	} {
		if _, err := tx.Exec(ctx, q, uid); err != nil {
			internal(w, "complete onboarding", err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		internal(w, "complete onboarding", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func internal(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
}
