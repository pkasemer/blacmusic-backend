// Package catalog serves track metadata and aggregated search.
package catalog

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/source"
)

// Handler serves catalog endpoints.
type Handler struct {
	pool *pgxpool.Pool
	reg  *source.Registry
}

// NewHandler creates the catalog handler.
func NewHandler(pool *pgxpool.Pool, reg *source.Registry) *Handler {
	return &Handler{pool: pool, reg: reg}
}

// Search fans out to every search-capable provider (optionally filtered with
// ?sources=native,audius) and concatenates the results.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "q is required")
		return
	}
	limit := intParam(r, "limit", 20, 1, 50)
	offset := intParam(r, "offset", 0, 0, 10000)

	var only map[string]bool
	if s := r.URL.Query().Get("sources"); s != "" {
		only = map[string]bool{}
		for _, name := range strings.Split(s, ",") {
			only[strings.TrimSpace(name)] = true
		}
	}

	tracks := []source.Track{}
	for _, p := range h.reg.All() {
		if only != nil && !only[p.Name()] {
			continue
		}
		if !source.Has(p, source.CapSearch) {
			continue
		}
		res, err := p.Search(r.Context(), q, limit, offset)
		if err != nil {
			slog.Warn("provider search failed", "source", p.Name(), "err", err)
			continue
		}
		tracks = append(tracks, res...)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"tracks": tracks})
}

// GetTrack returns a canonical track by Blac Music id.
func (h *Handler) GetTrack(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	uid, _ := auth.UserID(r.Context())
	var t source.Track
	// Drafts are visible to the artist who owns them, and to nobody else.
	err := h.pool.QueryRow(r.Context(), `
		SELECT t.id, t.title, COALESCE(t.artist_id, ''), COALESCE(a.name, ''), COALESCE(t.album_id, ''), COALESCE(al.title, ''),
		       t.duration_ms, COALESCE(t.artwork_url, al.artwork_url, '')
		FROM tracks t
		LEFT JOIN artists a  ON a.id  = t.artist_id
		LEFT JOIN albums  al ON al.id = t.album_id
		WHERE t.id = $1 AND (t.visibility = 'public' OR a.owner_user_id::text = $2)`, id, uid).
		Scan(&t.ID, &t.Title, &t.ArtistID, &t.ArtistName, &t.AlbumID, &t.AlbumTitle, &t.DurationMs, &t.ArtworkURL)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "not_found", "track not found")
		return
	}
	if err != nil {
		slog.Error("get track", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
		return
	}
	httpx.JSON(w, http.StatusOK, t)
}

func intParam(r *http.Request, key string, def, min, max int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min {
		return def
	}
	if n > max {
		return max
	}
	return n
}
