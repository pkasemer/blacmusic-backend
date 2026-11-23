// Package upload lets creators submit audio and tracks its processing.
//
// Flow: POST /v1/uploads reserves an id and returns a presigned PUT URL; the
// client sends the file straight to object storage; POST /v1/uploads/{id}/complete
// queues it; the worker (internal/worker) probes and transcodes it, then creates
// the track.
package upload

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Status is where an upload is in its life.
type Status string

const (
	StatusPending    Status = "pending"    // URL issued, file not confirmed yet
	StatusQueued     Status = "queued"     // waiting for a worker
	StatusProcessing Status = "processing" // a worker has it
	StatusReady      Status = "ready"      // track created and playable
	StatusFailed     Status = "failed"     // see Error
)

// MaxActivePerUser caps uploads that are not finished (pending, queued or processing).
const MaxActivePerUser = 20

var (
	ErrNotFound = errors.New("upload: not found")
	ErrLimit    = errors.New("upload: too many unfinished uploads")
)

// Upload is one submitted file.
type Upload struct {
	ID          string
	UserID      string
	TrackID     string // reserved at creation; the track exists once Status is ready
	Title       string
	ArtistName  string
	AlbumTitle  string
	Explicit    bool
	Filename    string
	SizeBytes   int64
	OriginalKey string
	Status      Status
	Error       string
	Attempts    int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Store is the database side of uploads and the processing queue.
type Store struct{ pool *pgxpool.Pool }

// NewStore creates the store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const cols = `id, user_id::text, track_id, title, artist_name, album_title, explicit, filename,
	size_bytes, original_key, status, COALESCE(error, ''), attempts, created_at, updated_at`

func scan(row pgx.Row) (*Upload, error) {
	var u Upload
	var st string
	err := row.Scan(&u.ID, &u.UserID, &u.TrackID, &u.Title, &u.ArtistName, &u.AlbumTitle, &u.Explicit,
		&u.Filename, &u.SizeBytes, &u.OriginalKey, &st, &u.Error, &u.Attempts, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	u.Status = Status(st)
	return &u, err
}

// NewUpload is the metadata supplied when reserving an upload.
type NewUpload struct {
	UserID, Title, ArtistName, AlbumTitle, Filename string
	Explicit                                        bool
	SizeBytes                                       int64
}

// Create reserves an upload. The unfinished-uploads cap is checked in the same
// statement; two simultaneous requests can overshoot it by a row, which is harmless.
func (s *Store) Create(ctx context.Context, n NewUpload) (*Upload, error) {
	u, err := scan(s.pool.QueryRow(ctx, `
		WITH ids AS (
			SELECT 'upl_' || replace(gen_random_uuid()::text, '-', '') AS id
		)
		INSERT INTO uploads (id, user_id, title, artist_name, album_title, explicit, filename, size_bytes, original_key)
		SELECT ids.id, $1, $2, $3, $4, $5, $6, $7, 'originals/' || ids.id
		FROM ids
		WHERE (SELECT count(*) FROM uploads
		       WHERE user_id = $1 AND status IN ('pending','queued','processing')) < $8
		RETURNING `+cols,
		n.UserID, n.Title, n.ArtistName, n.AlbumTitle, n.Explicit, n.Filename, n.SizeBytes, MaxActivePerUser))
	if errors.Is(err, ErrNotFound) { // no row inserted: cap reached
		return nil, ErrLimit
	}
	return u, err
}

// Get returns one upload.
func (s *Store) Get(ctx context.Context, id string) (*Upload, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM uploads WHERE id = $1`, id))
}

// ListByUser returns a user's uploads, newest first.
func (s *Store) ListByUser(ctx context.Context, userID string, limit, offset int) ([]*Upload, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM uploads WHERE user_id = $1
		ORDER BY created_at DESC, id LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Upload{}
	for rows.Next() {
		u, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Queue moves a pending upload to queued. Calling it again on an upload that
// is already past pending is a no-op that returns the current state.
func (s *Store) Queue(ctx context.Context, id string) (*Upload, error) {
	u, err := scan(s.pool.QueryRow(ctx, `
		UPDATE uploads SET status = 'queued', updated_at = now()
		WHERE id = $1 AND status = 'pending' RETURNING `+cols, id))
	if errors.Is(err, ErrNotFound) {
		return s.Get(ctx, id)
	}
	return u, err
}

// Fail marks an upload failed with a message that is safe to show the user.
func (s *Store) Fail(ctx context.Context, id, msg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE uploads SET status = 'failed', error = $2, locked_at = NULL, updated_at = now()
		WHERE id = $1 AND status IN ('pending','queued','processing')`, id, msg)
	return err
}

// Claim hands the oldest queued upload to a worker, or returns (nil, nil) when
// the queue is empty. A job whose worker died (processing for longer than
// staleAfter) is claimed again until it has used maxAttempts.
func (s *Store) Claim(ctx context.Context, staleAfter time.Duration, maxAttempts int) (*Upload, error) {
	// Retire jobs that keep killing their worker.
	if _, err := s.pool.Exec(ctx, `
		UPDATE uploads SET status = 'failed', error = 'processing did not finish, please upload again',
		       locked_at = NULL, updated_at = now()
		WHERE status = 'processing' AND attempts >= $2
		  AND locked_at < now() - make_interval(secs => $1)`,
		staleAfter.Seconds(), maxAttempts); err != nil {
		return nil, err
	}
	u, err := scan(s.pool.QueryRow(ctx, `
		UPDATE uploads SET status = 'processing', attempts = attempts + 1, locked_at = now(), updated_at = now()
		WHERE id = (
			SELECT id FROM uploads
			WHERE status = 'queued'
			   OR (status = 'processing' AND attempts < $2 AND locked_at < now() - make_interval(secs => $1))
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1)
		RETURNING `+cols, staleAfter.Seconds(), maxAttempts))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return u, err
}

// Requeue puts a job back after a transient failure.
func (s *Store) Requeue(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE uploads SET status = 'queued', locked_at = NULL, updated_at = now()
		WHERE id = $1 AND status = 'processing'`, id)
	return err
}

// Rendition is one transcoded file.
type Rendition struct {
	Quality     string
	MediaKey    string
	BitrateKbps int
	SizeBytes   int64
}

// Result is what a successful transcode produced.
type Result struct {
	DurationMs int
	Renditions []Rendition // must include "medium"
}

// Finish creates the artist, album and track, registers the renditions and
// marks the upload ready, all in one transaction.
func (s *Store) Finish(ctx context.Context, u *Upload, res Result) error {
	var medium *Rendition
	for i := range res.Renditions {
		if res.Renditions[i].Quality == "medium" {
			medium = &res.Renditions[i]
		}
	}
	if medium == nil {
		return fmt.Errorf("upload %s: no medium rendition", u.ID)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	artistID, err := findOrCreate(ctx, tx, "artist:"+u.ArtistName,
		`SELECT id FROM artists WHERE lower(name) = lower($1) ORDER BY created_at, id LIMIT 1`,
		`INSERT INTO artists (name) VALUES ($1) RETURNING id`, u.ArtistName)
	if err != nil {
		return err
	}

	var albumID *string
	if u.AlbumTitle != "" {
		id, err := findOrCreate(ctx, tx, "album:"+artistID+":"+u.AlbumTitle,
			`SELECT id FROM albums WHERE artist_id = $2 AND lower(title) = lower($1) ORDER BY created_at, id LIMIT 1`,
			`INSERT INTO albums (title, artist_id) VALUES ($1, $2) RETURNING id`, u.AlbumTitle, artistID)
		if err != nil {
			return err
		}
		albumID = &id
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO tracks (id, title, artist_id, album_id, duration_ms, explicit)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		u.TrackID, u.Title, artistID, albumID, res.DurationMs, u.Explicit); err != nil {
		return err
	}
	// Legacy single-file pointer: keeps the track visible to native search and
	// gives the playback fallback something to serve if renditions were lost.
	if _, err := tx.Exec(ctx, `
		INSERT INTO track_sources (track_id, source, source_id, media_key, codec, bitrate_kbps)
		VALUES ($1, 'native', $1, $2, 'aac', $3)`,
		u.TrackID, medium.MediaKey, medium.BitrateKbps); err != nil {
		return err
	}
	for _, r := range res.Renditions {
		if _, err := tx.Exec(ctx, `
			INSERT INTO track_renditions (track_id, quality, media_key, codec, container, bitrate_kbps, size_bytes)
			VALUES ($1, $2, $3, 'aac', 'm4a', $4, $5)`,
			u.TrackID, r.Quality, r.MediaKey, r.BitrateKbps, r.SizeBytes); err != nil {
			return err
		}
	}

	// Only the worker that still owns the job may finish it.
	tag, err := tx.Exec(ctx, `
		UPDATE uploads SET status = 'ready', error = NULL, locked_at = NULL, updated_at = now()
		WHERE id = $1 AND status = 'processing'`, u.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("upload %s is no longer being processed", u.ID)
	}
	return tx.Commit(ctx)
}

// findOrCreate looks an entity up by name and creates it when missing. An
// advisory lock keyed on lockKey stops two workers from creating the same
// artist or album at the same time.
func findOrCreate(ctx context.Context, tx pgx.Tx, lockKey, selectSQL, insertSQL string, args ...any) (string, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		return "", err
	}
	var id string
	err := tx.QueryRow(ctx, selectSQL, args...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, insertSQL, args...).Scan(&id)
	}
	return id, err
}
