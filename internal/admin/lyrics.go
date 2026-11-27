package admin

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/lyrics"
)

// Administrators write the lyrics, and their translations, of any song.
func (h *Handler) lyricsRoutes(r chi.Router) {
	r.Get("/tracks/{id}/lyrics", h.getLyrics)
	r.Put("/tracks/{id}/lyrics", h.putLyrics)
	r.Delete("/tracks/{id}/lyrics", h.deleteLyrics)
}

type adminLyrics struct {
	TrackID    string         `json:"trackId"`
	Title      string         `json:"title"`
	DurationMs int            `json:"durationMs"` // a time after this is refused
	Lyrics     *lyrics.Lyrics `json:"lyrics"`     // null when the song has none
	// UntranslatedLines is a warning, not an error: sung lines of timed lyrics without a translation.
	UntranslatedLines int `json:"untranslatedLines"`
}

func (h *Handler) lyricsView(ctx context.Context, trackID string) (*adminLyrics, error) {
	v := &adminLyrics{TrackID: trackID}
	if err := h.pool.QueryRow(ctx, `SELECT title, duration_ms FROM tracks WHERE id = $1`, trackID).Scan(&v.Title, &v.DurationMs); err != nil {
		return nil, err
	}
	l, err := lyrics.Of(ctx, h.pool, trackID)
	switch {
	case errors.Is(err, lyrics.ErrNotFound):
	case err != nil:
		return nil, err
	default:
		v.Lyrics, v.UntranslatedLines = l, l.Untranslated()
	}
	return v, nil
}

// GET /v1/admin/tracks/{id}/lyrics
func (h *Handler) getLyrics(w http.ResponseWriter, r *http.Request) {
	v, err := h.lyricsView(r.Context(), param(r, "id"))
	if err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, v)
}

// changeLyrics runs change with the song locked and records it.
func (h *Handler) changeLyrics(r *http.Request, action string, change func(tx pgx.Tx, durationMs int) (map[string]any, error)) error {
	ctx, id := r.Context(), param(r, "id")
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var title string
	var durationMs int
	if err := tx.QueryRow(ctx, `SELECT title, duration_ms FROM tracks WHERE id = $1 FOR UPDATE`, id).Scan(&title, &durationMs); err != nil {
		return err
	}
	details, err := change(tx, durationMs)
	if err != nil || details == nil {
		return err
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: action, TargetType: "track", TargetID: id, TargetLabel: title, Details: details}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PUT /v1/admin/tracks/{id}/lyrics {lrc? , text?}: the same rules as an artist's own save.
func (h *Handler) putLyrics(w http.ResponseWriter, r *http.Request) {
	var in lyrics.Input
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	err := h.changeLyrics(r, "track.lyrics_update", func(tx pgx.Tx, durationMs int) (map[string]any, error) {
		l, err := lyrics.Build(in, durationMs)
		if err != nil {
			return nil, err
		}
		if err := lyrics.Save(r.Context(), tx, param(r, "id"), l); err != nil {
			return nil, err
		}
		return map[string]any{"synced": l.Synced, "lines": len(l.Lines), "translated": len(l.Lines) > 0 && l.Untranslated() < len(l.Lines)}, nil
	})
	var ve *lyrics.ValidationError
	if errors.As(err, &ve) {
		lyrics.Invalid(w, ve)
		return
	}
	if err != nil {
		reply(w, err)
		return
	}
	h.getLyrics(w, r)
}

// DELETE /v1/admin/tracks/{id}/lyrics
func (h *Handler) deleteLyrics(w http.ResponseWriter, r *http.Request) {
	err := h.changeLyrics(r, "track.lyrics_delete", func(tx pgx.Tx, _ int) (map[string]any, error) {
		tag, err := tx.Exec(r.Context(), `DELETE FROM track_lyrics WHERE track_id = $1`, param(r, "id"))
		if err != nil || tag.RowsAffected() == 0 {
			return nil, err // nothing there: nothing to record
		}
		return map[string]any{}, nil
	})
	if err != nil {
		reply(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
