package catalog

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/recommend"
	"github.com/blacmusic/backend/internal/source"
)

const (
	mixDefault     = 25
	mixMax         = 50
	mixMaxExcluded = 200
)

// Mix suggests songs to play after one song, for "Start mix" and autoplay. The recommendation engine ranks songs like
// it (never padded with other genres) by that likeness and the listener's taste, in a somewhat different order each
// time. What was asked and suggested is kept.
//
//	POST /v1/mixes {trackId, reason: "mix"|"autoplay", excludeTrackIds?, limit?, userId?}
//
// userId is accepted from the app and ignored: the listener is who the token says.
func (h *Handler) Mix(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TrackID         string   `json:"trackId"`
		UserID          string   `json:"userId"`
		Reason          string   `json:"reason"`
		ExcludeTrackIDs []string `json:"excludeTrackIds"`
		Limit           int      `json:"limit"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	switch {
	case in.TrackID == "":
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", "trackId is required")
		return
	case in.Reason != "mix" && in.Reason != "autoplay":
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", "reason must be mix or autoplay")
		return
	case len(in.ExcludeTrackIDs) > mixMaxExcluded:
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", "excludeTrackIds takes at most 200 ids")
		return
	}
	if in.Limit <= 0 {
		in.Limit = mixDefault
	}
	in.Limit = min(in.Limit, mixMax)

	ctx := r.Context()
	var genre string
	err := h.pool.QueryRow(ctx, `SELECT COALESCE(NULLIF(t.genre, ''), al.genre, '') FROM tracks t LEFT JOIN albums al ON al.id = t.album_id
		WHERE t.id = $1 AND t.visibility = 'public'`, in.TrackID).Scan(&genre)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "not_found", "track not found")
		return
	}
	if err != nil {
		internalError(w, "mix: reference track", err)
		return
	}
	exclude := append([]string{in.TrackID}, in.ExcludeTrackIDs...)
	tracks, err := h.rankedMix(ctx, r, in.TrackID, in.Reason, exclude, in.Limit)
	if err != nil {
		// the listener still gets a mix: songs of the same genre, as before the engine
		slog.Error("mix: ranking, using the same genre", "err", err)
		// every artist's first pick comes before anyone's second, so one artist does not fill the mix
		tracks, err = h.queryTracks(ctx,
			publicNativeTrack+` AND $1 <> '' AND lower(COALESCE(NULLIF(t.genre, ''), al.genre)) = lower($1) AND NOT (t.id = ANY($2))
			AND NOT EXISTS (SELECT 1 FROM rec_blocked_tracks b WHERE b.track_id = t.id)`,
			[]any{genre, exclude}, "row_number() OVER (PARTITION BY t.artist_id ORDER BY random()), random()", in.Limit, 0)
	}
	if err != nil {
		internalError(w, "mix", err)
		return
	}
	h.keepMix(ctx, r, in.TrackID, in.Reason, func() []string {
		ids := make([]string, len(tracks))
		for i, t := range tracks {
			ids[i] = t.ID
		}
		return ids
	}())
	httpx.JSON(w, http.StatusOK, map[string]any{"tracks": tracks})
}

// rankedMix asks the engine, reads the songs it chose in its order, and keeps what was suggested and why.
func (h *Handler) rankedMix(ctx context.Context, r *http.Request, trackID, reason string, exclude []string, limit int) ([]source.Track, error) {
	uid, _ := auth.UserID(r.Context())
	// a new variation each time: the same request twice need not give the same list
	res, err := recommend.Mix(ctx, h.pool, uid, trackID, exclude, limit, strconv.FormatInt(time.Now().UnixNano(), 36))
	if errors.Is(err, recommend.ErrNoSong) {
		return []source.Track{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(res.Picks) == 0 {
		return []source.Track{}, nil
	}
	ids := make([]string, len(res.Picks))
	for i, p := range res.Picks {
		ids[i] = p.TrackID
	}
	tracks, err := h.queryTracks(ctx, publicNativeTrack+` AND t.id = ANY($1)`, []any{ids}, "array_position($1::text[], t.id)", len(ids), 0)
	if err != nil {
		return nil, err
	}
	if err := recommend.Record(ctx, h.pool, uid, reason, res.Mode, trackID, res.Picks); err != nil {
		slog.Warn("suggestions not recorded", "err", err)
	}
	return tracks, nil
}

// keepMix records a request. A failure never costs the listener their mix.
func (h *Handler) keepMix(ctx context.Context, r *http.Request, trackID, reason string, suggested []string) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		return
	}
	if _, err := h.pool.Exec(ctx, `INSERT INTO mix_requests (user_id, track_id, reason, suggested_ids) VALUES ($1, $2, $3, $4)`,
		uid, trackID, reason, suggested); err != nil {
		slog.Warn("mix request not recorded", "err", err)
	}
}
