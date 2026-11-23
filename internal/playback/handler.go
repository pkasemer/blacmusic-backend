// Package playback turns a track id into a PlaybackDescriptor by asking the
// track's sources, in priority order, until one can play it.
package playback

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
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
	TrackID   string                     `json:"trackId,omitempty"`
	EpisodeID string                     `json:"episodeId,omitempty"`
	Source    string                     `json:"source"`
	Playback  *source.PlaybackDescriptor `json:"playback"`
}

// episodePrefix marks podcast episode ids (epi_...); tracks are trk_...
const episodePrefix = "epi_"

type candidate struct{ source, sourceID string }

func (h *Handler) Resolve(w http.ResponseWriter, r *http.Request) {
	trackID := chi.URLParam(r, "id")
	quality, ok := media.ParseQuality(r.URL.Query().Get("quality"))
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "quality must be low, medium or high")
		return
	}
	opts := source.PlaybackOptions{Quality: string(quality)}

	isEpisode := strings.HasPrefix(trackID, episodePrefix)
	var cands []candidate
	var err error
	if isEpisode {
		cands = []candidate{{source: "podcast", sourceID: trackID}} // episodes have exactly one source
	} else {
		uid, _ := auth.UserID(r.Context())
		cands, err = h.candidates(r.Context(), trackID, uid)
	}
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
			if isEpisode && errors.Is(err, source.ErrNotFound) {
				httpx.Error(w, http.StatusNotFound, "not_found", "episode not found")
				return
			}
			slog.Warn("resolve playback failed", "source", c.source, "track", trackID, "err", err)
			continue // fall back to the next source
		}
		resp := response{Source: c.source, Playback: desc}
		if isEpisode {
			resp.EpisodeID = trackID
		} else {
			resp.TrackID = trackID
		}
		httpx.JSON(w, http.StatusOK, resp)
		return
	}
	httpx.Error(w, http.StatusBadGateway, "no_playable_source", "no enabled source could play this track")
}

// candidates lists a track's sources. A track that is not public is playable
// only by the artist who owns it (to preview a draft).
func (h *Handler) candidates(ctx context.Context, trackID, viewer string) ([]candidate, error) {
	rows, err := h.pool.Query(ctx, `
		SELECT ts.source, ts.source_id FROM track_sources ts
		JOIN tracks t ON t.id = ts.track_id
		LEFT JOIN artists a ON a.id = t.artist_id
		WHERE ts.track_id = $1 AND (t.visibility = 'public' OR a.owner_user_id::text = $2)
		ORDER BY ts.priority, ts.source`, trackID, viewer)
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
