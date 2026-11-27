// Package stats records plays and gives artists the numbers about their music: plays, listeners, monthly
// listeners, where plays came from, per release and per track.
package stats

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Play rules.
const (
	// MinListenSec is how long a track must play to count (shorter tracks: half of them).
	MinListenSec = 30
	// ShortTrackSec: tracks shorter than this count after half their length.
	ShortTrackSec = 60
	// DedupeSec: the same user playing the same track again within this long counts once.
	DedupeSec = 30
	// MaxListenSec bounds what the app may report for one play.
	MaxListenSec = 24 * 60 * 60
)

// Sources the app may report where a play started; anything else is stored as "other".
var Sources = map[string]bool{
	"search": true, "album": true, "playlist": true, "artist": true, "library": true,
	"radio": true, "downloads": true, "home": true, "other": true, "autoplay": true,
}

// ErrTrackNotFound means the track does not exist or is not public.
var ErrTrackNotFound = errors.New("stats: track not found")

// ValidationError is a bad request.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// PlayInput is what the app reports.
type PlayInput struct {
	TrackID     string `json:"trackId"`
	ListenedSec int    `json:"listenedSec"`
	Source      string `json:"source"`
}

// Store records and reads plays.
type Store struct{ pool *pgxpool.Pool }

// NewStore creates the store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Counts says whether a listen of listenedSec seconds is a play for a track of durationSec seconds.
func Counts(listenedSec, durationSec int) bool {
	if durationSec > 0 && durationSec < ShortTrackSec {
		return listenedSec*2 >= durationSec
	}
	return listenedSec >= MinListenSec
}

// Record stores a play. It reports whether a row was stored: listens that are too short, an artist
// playing their own track, and the same user repeating the same track within DedupeSec are all
// accepted but not counted.
func (s *Store) Record(ctx context.Context, userID string, in PlayInput) (bool, error) {
	if in.TrackID == "" {
		return false, &ValidationError{"trackId is required"}
	}
	if in.ListenedSec < 0 || in.ListenedSec > MaxListenSec {
		return false, &ValidationError{"listenedSec must be between 0 and 86400"}
	}
	source := in.Source
	if !Sources[source] {
		source = "other"
	}
	var durationMs int
	err := s.pool.QueryRow(ctx, `SELECT duration_ms FROM tracks WHERE id = $1 AND visibility = 'public'`, in.TrackID).Scan(&durationMs)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrTrackNotFound
	}
	if err != nil {
		return false, err
	}
	durationSec := durationMs / 1000
	if !Counts(in.ListenedSec, durationSec) {
		return false, nil
	}
	completed := durationSec > 0 && in.ListenedSec*10 >= durationSec*9
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO plays (user_id, track_id, release_id, artist_id, listened_sec, completed, source)
		SELECT $1::uuid, t.id, t.album_id, t.artist_id, $3, $4, $5
		FROM tracks t LEFT JOIN artists a ON a.id = t.artist_id
		WHERE t.id = $2 AND t.visibility = 'public'
		  AND (a.owner_user_id IS NULL OR a.owner_user_id <> $1::uuid)
		  AND NOT EXISTS (SELECT 1 FROM plays p WHERE p.user_id = $1::uuid AND p.track_id = t.id
		                  AND p.at > now() - make_interval(secs => $6))`,
		userID, in.TrackID, in.ListenedSec, completed, source, DedupeSec)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
