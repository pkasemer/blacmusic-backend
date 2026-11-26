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
	"github.com/blacmusic/backend/internal/playlist"
	"github.com/blacmusic/backend/internal/source"
	"github.com/blacmusic/backend/internal/trackview"
)

// Handler serves /v1/me/library/*. All routes require authentication.
type Handler struct {
	pool      *pgxpool.Pool
	playlists *playlist.Store
	// PublicURL builds artist picture links in track lists (set by the server).
	PublicURL string
}

// NewHandler creates the handler.
func NewHandler(pool *pgxpool.Pool, playlists *playlist.Store) *Handler {
	return &Handler{pool: pool, playlists: playlists}
}

// kind describes one library table. Values are compile-time constants only;
// they are interpolated into SQL.
type kind struct {
	name   string // URL segment
	table  string // library_*
	col    string // entity id column
	entity string // entity table
	// visible is a SQL condition (on the entity table, aliased e) that a new
	// library entry must satisfy, so drafts cannot be saved.
	visible string
}

var (
	tracks  = kind{"tracks", "library_tracks", "track_id", "tracks", "e.visibility = 'public'"}
	albums  = kind{"albums", "library_albums", "album_id", "albums", "e.visibility = 'public'"}
	artists = kind{"artists", "library_artists", "artist_id", "artists", "true"}
	// Only public playlists can be saved: a private one is not for others.
	playlists = kind{"playlists", "library_playlists", "playlist_id", "playlists", "e.is_public"}
)

const maxContainsIDs = 100

// Routes registers the library routes on r (already authenticated).
func (h *Handler) Routes(r chi.Router) {
	for _, k := range []kind{tracks, albums, artists, playlists} {
		k := k
		r.Get("/"+k.name+"/contains", h.contains(k))
		r.Put("/"+k.name+"/{id}", h.add(k))
		r.Delete("/"+k.name+"/{id}", h.remove(k))
	}
	r.Get("/tracks", h.listTracks)
	r.Get("/albums", h.listAlbums)
	r.Get("/artists", h.listArtists)
	r.Get("/playlists", h.listPlaylists)
}

func (h *Handler) add(k kind) http.HandlerFunc {
	exists := "SELECT EXISTS (SELECT 1 FROM " + k.entity + " e WHERE e.id = $1 AND " + k.visible + ")"
	q := "INSERT INTO " + k.table + " (user_id, " + k.col + ") VALUES ($1, $2) ON CONFLICT DO NOTHING"
	return func(w http.ResponseWriter, r *http.Request) {
		uid, _ := auth.UserID(r.Context())
		id := chi.URLParam(r, "id")
		notFound := func() {
			httpx.Error(w, http.StatusNotFound, "not_found", strings.TrimSuffix(k.name, "s")+" not found")
		}
		var ok bool
		if err := h.pool.QueryRow(r.Context(), exists, id).Scan(&ok); err != nil {
			serverError(w, err)
			return
		}
		if !ok {
			notFound()
			return
		}
		_, err := h.pool.Exec(r.Context(), q, uid, id)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			notFound() // deleted between the check and the insert
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
	// the same track shape as search and playlists, plus when it was saved
	rows, err := h.pool.Query(r.Context(), trackview.Select("l.added_at")+`
		JOIN library_tracks l ON l.track_id = t.id
		WHERE l.user_id = $1 AND t.visibility = 'public'
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
		var err error
		if s.Track, err = trackview.Scan(rows, h.PublicURL, &s.AddedAt); err != nil {
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
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	ArtistID       string    `json:"artistId,omitempty"`
	ArtistName     string    `json:"artistName,omitempty"`
	ArtistVerified bool      `json:"artistVerified"`
	ArtworkURL     string    `json:"artworkUrl,omitempty"`
	AddedAt        time.Time `json:"addedAt"`
}

func (h *Handler) listAlbums(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, offset := page(r)
	rows, err := h.pool.Query(r.Context(), `
		SELECT al.id, al.title, COALESCE(a.id, ''), COALESCE(a.name, ''), COALESCE(a.verification = 'verified', false), COALESCE(al.artwork_url, ''), l.added_at
		FROM library_albums l
		JOIN albums al      ON al.id = l.album_id
		LEFT JOIN artists a ON a.id  = al.artist_id
		WHERE l.user_id = $1 AND al.visibility = 'public'
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
		if err := rows.Scan(&s.ID, &s.Title, &s.ArtistID, &s.ArtistName, &s.ArtistVerified, &s.ArtworkURL, &s.AddedAt); err != nil {
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
	Verified bool      `json:"verified"`
	AddedAt  time.Time `json:"addedAt"`
}

func (h *Handler) listArtists(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, offset := page(r)
	rows, err := h.pool.Query(r.Context(), `
		SELECT a.id, a.name, COALESCE(a.image_url, ''), a.verification = 'verified', l.added_at
		FROM library_artists l
		JOIN artists a ON a.id = l.artist_id
		WHERE l.user_id = $1 AND a.banned_at IS NULL
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
		if err := rows.Scan(&s.ID, &s.Name, &s.ImageURL, &s.Verified, &s.AddedAt); err != nil {
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

func (h *Handler) listPlaylists(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, offset := page(r)
	ps, err := h.playlists.ListSaved(r.Context(), uid, limit, offset)
	if err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"playlists": ps})
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
