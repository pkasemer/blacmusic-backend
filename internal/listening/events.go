// Package listening keeps how people listen and, from it, each listener's "Your sound" playlist.
package listening

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
)

// Handler serves the listening routes (authenticated).
type Handler struct{ pool *pgxpool.Pool }

// NewHandler creates the handler.
func NewHandler(pool *pgxpool.Pool) *Handler { return &Handler{pool: pool} }

const (
	maxBatch  = 50
	maxID     = 64
	maxListen = 86400
)

var sources = map[string]bool{
	"": true, "playlist": true, "album": true, "artist": true, "podcast": true, "song": true, "release": true, "search": true,
	"genre": true, "mix": true, "favourites": true, "downloads": true, "recent": true, "trending": true, "autoplay": true,
}

// endReasons say how a listen ended. next, jump and replaced are the listener moving on; ended is the song running
// out; unobserved is a change the app did not see happen, which must never be read as a skip.
var endReasons = map[string]bool{
	"ended": true, "next": true, "previous": true, "jump": true, "replaced": true, "restart": true, "removed": true,
	"stopped": true, "paused": true, "sign_out": true, "app_closed": true, "unobserved": true,
}

type event struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	At          string `json:"at"`
	SessionID   string `json:"sessionId"`
	TrackID     string `json:"trackId"`
	PlaylistID  string `json:"playlistId"`
	ListenedSec int    `json:"listenedSec"`
	DurationSec int    `json:"durationSec"`
	Completion  int    `json:"completion"`
	Outcome     string `json:"outcome"`
	Replay      bool   `json:"replay"`
	Source      string `json:"source"`

	// sent since capture 2; older apps leave them out
	EndReason   string `json:"endReason"`
	PositionSec int    `json:"positionSec"`
	Autoplay    bool   `json:"autoplay"` // autoplay chose the song; a flag, so a server without it still keeps the listen
	SeedTrackID string `json:"autoplaySeedId"`
	Capture     int    `json:"capture"`
}

// tidy puts right what the newer fields say. A bad one is dropped, never the listen it belongs to.
func (e *event) tidy() {
	if !endReasons[e.EndReason] {
		e.EndReason = ""
	}
	if e.PositionSec < 0 || e.PositionSec > maxListen {
		e.PositionSec = 0
	}
	if e.Capture < 0 || e.Capture > 1000 {
		e.Capture = 0
	}
	if e.Autoplay {
		e.Source = "autoplay" // its own, not that of the list the queue started from
	}
	if e.Source != "autoplay" || len(e.SeedTrackID) > maxID {
		e.SeedTrackID = ""
	}
	// a song heard through was not skipped, nor was one whose end nobody saw
	if e.Outcome == "skipped" {
		switch {
		case e.Completion >= 90:
			e.Outcome = "completed"
		case e.EndReason == "ended" || e.EndReason == "unobserved" || e.EndReason == "app_closed":
			e.Outcome = "stopped"
		}
	}
}

// check says whether an event makes sense, and gives its time.
func (e *event) check(now time.Time) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339, e.At)
	// events wait on the phone while it is offline, so old ones are fine; a clock far ahead is not
	if err != nil || at.After(now.Add(24*time.Hour)) || at.Before(now.AddDate(-1, 0, 0)) {
		return at, false
	}
	if e.ID == "" || len(e.ID) > maxID || len(e.SessionID) > maxID || len(e.PlaylistID) > maxID || len(e.TrackID) > maxID || !sources[e.Source] {
		return at, false
	}
	needsTrack := false
	switch e.Type {
	case "listen":
		needsTrack = true
		if e.Outcome != "completed" && e.Outcome != "skipped" && e.Outcome != "stopped" || e.SessionID == "" ||
			e.ListenedSec < 0 || e.ListenedSec > maxListen || e.DurationSec < 0 || e.DurationSec > maxListen || e.Completion < 0 || e.Completion > 100 {
			return at, false
		}
	case "like", "unlike":
		needsTrack = true
	case "playlist_add":
		needsTrack = true
		if e.PlaylistID == "" {
			return at, false
		}
	case "session_start", "session_end":
		if e.SessionID == "" {
			return at, false
		}
	default:
		return at, false
	}
	// only the fields that belong to the type are kept
	if e.Type != "listen" {
		e.ListenedSec, e.DurationSec, e.Completion, e.Outcome, e.Replay, e.Source = 0, 0, 0, "", false, ""
		e.EndReason, e.PositionSec, e.SeedTrackID = "", 0, ""
	}
	if !needsTrack {
		e.TrackID = ""
	}
	if e.Type != "playlist_add" {
		e.PlaylistID = ""
	}
	return at, !needsTrack || e.TrackID != ""
}

// Events is POST /v1/me/listening-events {events:[...]} -> 204. The app throws a refused batch away, so one bad event
// is left out rather than refusing the rest; only a body that cannot be read is refused. These are not plays.
func (h *Handler) Events(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil || in.Events == nil || len(in.Events) > maxBatch {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "send {events: [...]} with at most 50 events")
		return
	}
	ctx := r.Context()
	uid, _ := auth.UserID(ctx)
	skipped := 0
	for _, raw := range in.Events {
		var e event
		if err := json.Unmarshal(raw, &e); err != nil {
			skipped++
			continue
		}
		e.tidy()
		at, ok := e.check(time.Now())
		if !ok {
			skipped++
			continue
		}
		// an event about a song that is not here is left out; one already stored stays as it was
		if _, err := h.pool.Exec(ctx, `
			INSERT INTO listening_events (user_id, event_id, type, at, track_id, session_id, playlist_id, listened_sec, duration_sec, completion, outcome, replay, source,
			                              end_reason, end_position_sec, seed_track_id, capture)
			SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
			WHERE $5 = '' OR EXISTS (SELECT 1 FROM tracks WHERE id = $5)
			ON CONFLICT (user_id, event_id) DO NOTHING`,
			uid, e.ID, e.Type, at, e.TrackID, e.SessionID, e.PlaylistID, e.ListenedSec, e.DurationSec, e.Completion, e.Outcome, e.Replay, e.Source,
			e.EndReason, e.PositionSec, e.SeedTrackID, e.Capture); err != nil {
			internal(w, "listening events", err) // the app keeps the batch and tries again
			return
		}
	}
	if skipped > 0 {
		slog.Info("listening events left out", "count", skipped)
	}
	w.WriteHeader(http.StatusNoContent)
}

func internal(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
}
