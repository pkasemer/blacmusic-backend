// Package playback turns a track id into a PlaybackDescriptor by asking the
// track's sources, in priority order, until one can play it.
package playback

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/media"
	"github.com/blacmusic/backend/internal/source"
)

// Handler serves GET /v1/playback/{id}?quality=low|medium|high.
type Handler struct {
	pool *pgxpool.Pool
	reg  *source.Registry
}

// NewHandler creates the playback handler.
func NewHandler(pool *pgxpool.Pool, reg *source.Registry) *Handler {
	return &Handler{pool: pool, reg: reg}
}

type response struct {
	TrackID  string                     `json:"trackId"`
	Source   string                     `json:"source"`
	Playback *source.PlaybackDescriptor `json:"playback"`
}

type candidate struct{ source, sourceID string }

func (h *Handler) Resolve(w http.ResponseWriter, r *http.Request) {
	trackID := chi.URLParam(r, "id")
	quality, ok := media.ParseQuality(r.URL.Query().Get("quality"))
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "quality must be low, medium or high")
		return
	}
	opts := source.PlaybackOptions{Quality: string(quality)}

	cands, err := h.candidates(r.Context(), trackID)
	if err != nil {
		slog.Error("playback candidates", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
		return
	}
	if len(cands) == 0 {
		httpx.Error(w, http.StatusNotFound, "not_found", "track not found or has no playable source")
		return
	}

	for _, c := range cands {
		p, ok := h.reg.Get(c.source)
		if !ok {
			continue // source disabled on this deployment
		}
		desc, err := p.ResolvePlayback(r.Context(), c.sourceID, opts)
		if err != nil {
			slog.Warn("resolve playback failed", "source", c.source, "track", trackID, "err", err)
			continue // fall back to the next source
		}
		httpx.JSON(w, http.StatusOK, response{TrackID: trackID, Source: c.source, Playback: desc})
		return
	}
	httpx.Error(w, http.StatusBadGateway, "no_playable_source", "no enabled source could play this track")
}

func (h *Handler) candidates(ctx context.Context, trackID string) ([]candidate, error) {
	rows, err := h.pool.Query(ctx,
		`SELECT source, source_id FROM track_sources WHERE track_id = $1 ORDER BY priority, source`, trackID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.source, &c.sourceID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
