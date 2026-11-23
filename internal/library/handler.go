// Package library implements the user's saved tracks, albums and followed artists.
package library

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/source"
)

// Handler serves /v1/me/library/*. All routes require authentication.
type Handler struct{ pool *pgxpool.Pool }

// NewHandler creates the handler.
func NewHandler(pool *pgxpool.Pool) *Handler { return &Handler{pool: pool} }

// kind describes one library table. Values are compile-time constants only;
// they are interpolated into SQL.
type kind struct {
	name   string // URL segment
	table  string // library_*
	col    string // entity id column
	entity string // entity table
}

var (
	tracks  = kind{"tracks", "library_tracks", "track_id", "tracks"}
	albums  = kind{"albums", "library_albums", "album_id", "albums"}
	artists = kind{"artists", "library_artists", "artist_id", "artists"}
)

const maxContainsIDs = 100

// Routes registers the library routes on r (already authenticated).
func (h *Handler) Routes(r chi.Router) {
	for _, k := range []kind{tracks, albums, artists} {
		k := k
		r.Get("/"+k.name+"/contains", h.contains(k))
		r.Put("/"+k.name+"/{id}", h.add(k))
		r.Delete("/"+k.name+"/{id}", h.remove(k))
	}
	r.Get("/tracks", h.listTracks)
	r.Get("/albums", h.listAlbums)
	r.Get("/artists", h.listArtists)
}

func (h *Handler) add(k kind) http.HandlerFunc {
	q := "INSERT INTO " + k.table + " (user_id, " + k.col + ") VALUES ($1, $2) ON CONFLICT DO NOTHING"
	return func(w http.ResponseWriter, r *http.Request) {
		uid, _ := auth.UserID(r.Context())
		_, err := h.pool.Exec(r.Context(), q, uid, chi.URLParam(r, "id"))
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			httpx.Error(w, http.StatusNotFound, "not_found", strings.TrimSuffix(k.name, "s")+" not found")
			return
		}
		if err != nil {
			serverError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *Handler) remove(k kind) http.HandlerFunc {
	q := "DELETE FROM " + k.table + " WHERE user_id = $1 AND " + k.col + " = $2"
	return func(w http.ResponseWriter, r *http.Request) {
		uid, _ := auth.UserID(r.Context())
		if _, err := h.pool.Exec(r.Context(), q, uid, chi.URLParam(r, "id")); err != nil {
			serverError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent) // idempotent
	}
}

// contains answers "which of these ids are in my library?" so the app can draw
// heart icons for a whole list with one request: ?ids=a,b,c
func (h *Handler) contains(k kind) http.HandlerFunc {
	q := "SELECT " + k.col + " FROM " + k.table + " WHERE user_id = $1 AND " + k.col + " = ANY($2)"
	return func(w http.ResponseWriter, r *http.Request) {
		uid, _ := auth.UserID(r.Context())
		var ids []string
		for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 || len(ids) > maxContainsIDs {
			httpx.Error(w, http.StatusBadRequest, "bad_request", "ids must contain 1-100 comma-separated ids")
			return
		}
		rows, err := h.pool.Query(r.Context(), q, uid, ids)
		if err != nil {
			serverError(w, err)
			return
		}
		defer rows.Close()
		result := make(map[string]bool, len(ids))
		for _, id := range ids {
			result[id] = false
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				serverError(w, err)
				return
			}
			result[id] = true
		}
		if err := rows.Err(); err != nil {
			serverError(w, err)
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"contains": result})
	}
}

type savedTrack struct {
	source.Track
	AddedAt time.Time `json:"addedAt"`
}

func (h *Handler) listTracks(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, offset := page(r)
	rows, err := h.pool.Query(r.Context(), `
		SELECT t.id, t.title, COALESCE(a.name, ''), COALESCE(al.title, ''),
		       t.duration_ms, COALESCE(t.artwork_url, al.artwork_url, ''), l.added_at
		FROM library_tracks l
		JOIN tracks t        ON t.id  = l.track_id
		LEFT JOIN artists a  ON a.id  = t.artist_id
		LEFT JOIN albums  al ON al.id = t.album_id
		WHERE l.user_id = $1
		ORDER BY l.added_at DESC, t.id
		LIMIT $2 OFFSET $3`, uid, limit, offset)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	out := []savedTrack{}
	for rows.Next() {
		var s savedTrack
		if err := rows.Scan(&s.ID, &s.Title, &s.ArtistName, &s.AlbumTitle, &s.DurationMs, &s.ArtworkURL, &s.AddedAt); err != nil {
			serverError(w, err)
			return
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"tracks": out})
}

type savedAlbum struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	ArtistName string    `json:"artistName,omitempty"`
	ArtworkURL string    `json:"artworkUrl,omitempty"`
	AddedAt    time.Time `json:"addedAt"`
}

func (h *Handler) listAlbums(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, offset := page(r)
	rows, err := h.pool.Query(r.Context(), `
		SELECT al.id, al.title, COALESCE(a.name, ''), COALESCE(al.artwork_url, ''), l.added_at
		FROM library_albums l
		JOIN albums al      ON al.id = l.album_id
		LEFT JOIN artists a ON a.id  = al.artist_id
		WHERE l.user_id = $1
		ORDER BY l.added_at DESC, al.id
		LIMIT $2 OFFSET $3`, uid, limit, offset)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	out := []savedAlbum{}
	for rows.Next() {
		var s savedAlbum
		if err := rows.Scan(&s.ID, &s.Title, &s.ArtistName, &s.ArtworkURL, &s.AddedAt); err != nil {
			serverError(w, err)
			return
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"albums": out})
}

type followedArtist struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	ImageURL string    `json:"imageUrl,omitempty"`
	AddedAt  time.Time `json:"addedAt"`
}

func (h *Handler) listArtists(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, offset := page(r)
	rows, err := h.pool.Query(r.Context(), `
		SELECT a.id, a.name, COALESCE(a.image_url, ''), l.added_at
		FROM library_artists l
		JOIN artists a ON a.id = l.artist_id
		WHERE l.user_id = $1
		ORDER BY l.added_at DESC, a.id
		LIMIT $2 OFFSET $3`, uid, limit, offset)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	out := []followedArtist{}
	for rows.Next() {
		var s followedArtist
		if err := rows.Scan(&s.ID, &s.Name, &s.ImageURL, &s.AddedAt); err != nil {
			serverError(w, err)
			return
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"artists": out})
}

func page(r *http.Request) (limit, offset int) {
	limit, offset = 50, 0
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, 200)
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		offset = min(n, 100000)
	}
	return
}

func serverError(w http.ResponseWriter, err error) {
	slog.Error("library handler", "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
}
