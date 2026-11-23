// Package upload lets artists submit audio and tracks its processing.
//
// Flow: POST /v1/uploads reserves an id (and quota) and returns a presigned PUT
// URL; the client sends the file straight to object storage; POST
// /v1/uploads/{id}/complete queues it; the worker (internal/worker) probes and
// transcodes it, then creates the track inside the upload's release.
package upload

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/artist"
)

// Status is where an upload is in its life.
type Status string

const (
	StatusPending    Status = "pending"    // URL issued, file not confirmed yet
	StatusQueued     Status = "queued"     // waiting for a worker
	StatusProcessing Status = "processing" // a worker has it
	StatusReady      Status = "ready"      // track created
	StatusFailed     Status = "failed"     // see Error
)

const (
	// MaxActivePerArtist caps uploads that are not finished (pending, queued or processing).
	MaxActivePerArtist = 20
	// MaxDailyUnverified caps how many uploads an artist without the verified
	// badge may start in 24 hours. It limits damage from throwaway accounts.
	MaxDailyUnverified = 30
)

var (
	ErrNotFound        = errors.New("upload: not found")
	ErrLimit           = errors.New("upload: too many unfinished uploads")
	ErrDailyLimit      = errors.New("upload: daily upload limit reached")
	ErrNoArtist        = errors.New("upload: this account has no artist profile")
	ErrReleaseNotFound = errors.New("upload: release not found")
	ErrInUse           = errors.New("upload: cannot be removed in its current state")
)

// QuotaError is returned when a file does not fit in the artist's storage.
type QuotaError struct {
	Required, Remaining, Limit, Used int64
}

func (e *QuotaError) Error() string { return "storage quota exceeded" }

// Upload is one submitted file.
type Upload struct {
	ID          string
	UserID      string
	ArtistID    string // empty on uploads from before artist mode
	ReleaseID   string // empty on uploads from before artist mode
	TrackID     string // reserved at creation; the track exists once Status is ready
	Title       string
	ArtistName  string
	AlbumTitle  string
	Genre       string
	TrackNumber *int
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

const cols = `id, user_id::text, COALESCE(artist_id, ''), COALESCE(release_id, ''), track_id, title, artist_name, album_title,
	genre, track_number, explicit, filename, size_bytes, original_key, status, COALESCE(error, ''), attempts, created_at, updated_at`

func scan(row pgx.Row) (*Upload, error) {
	var u Upload
	var st string
	err := row.Scan(&u.ID, &u.UserID, &u.ArtistID, &u.ReleaseID, &u.TrackID, &u.Title, &u.ArtistName, &u.AlbumTitle,
		&u.Genre, &u.TrackNumber, &u.Explicit, &u.Filename, &u.SizeBytes, &u.OriginalKey, &st, &u.Error, &u.Attempts,
		&u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	u.Status = Status(st)
	return &u, err
}

// NewUpload is the metadata supplied when reserving an upload.
type NewUpload struct {
	UserID       string
	ReleaseID    string // empty: a new draft single is created for this track
	Title        string
	Genre        string
	TrackNumber  *int
	Explicit     bool
	Filename     string
	SizeBytes    int64
	TermsVersion string // the terms the artist confirmed they own the rights under
}

// Create reserves an upload for the user's artist profile. In one transaction,
// holding a lock on the artist row, it checks the unfinished and daily caps and
// the storage quota, creates the draft single when no release was named, and
// inserts the upload, so parallel uploads can never overshoot the quota.
func (s *Store) Create(ctx context.Context, n NewUpload) (*Upload, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var artistID, artistName string
	var verified bool
	err = tx.QueryRow(ctx, `SELECT id, name, verification = 'verified' FROM artists WHERE owner_user_id = $1 FOR UPDATE`, n.UserID).
		Scan(&artistID, &artistName, &verified)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoArtist
	}
	if err != nil {
		return nil, err
	}

	var active, today int
	if err := tx.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE status IN ('pending','queued','processing')),
		count(*) FILTER (WHERE created_at > now() - interval '24 hours')
		FROM uploads WHERE artist_id = $1`, artistID).Scan(&active, &today); err != nil {
		return nil, err
	}
	if active >= MaxActivePerArtist {
		return nil, ErrLimit
	}
	if !verified && today >= MaxDailyUnverified {
		return nil, ErrDailyLimit
	}

	q, err := artist.QuotaFor(ctx, tx, artistID)
	if err != nil {
		return nil, err
	}
	if n.SizeBytes > q.RemainingStorageBytes {
		return nil, &QuotaError{Required: n.SizeBytes, Remaining: q.RemainingStorageBytes, Limit: q.TotalStorageBytes, Used: q.UsedStorageBytes}
	}

	var releaseID, releaseTitle string
	if n.ReleaseID == "" {
		err = tx.QueryRow(ctx, `
			INSERT INTO albums (title, artist_id, release_type, genre, visibility) VALUES ($1, $2, 'single', $3, 'draft')
			RETURNING id, title`, n.Title, artistID, n.Genre).Scan(&releaseID, &releaseTitle)
	} else {
		err = tx.QueryRow(ctx, `SELECT id, title FROM albums WHERE id = $1 AND artist_id = $2`, n.ReleaseID, artistID).
			Scan(&releaseID, &releaseTitle)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrReleaseNotFound
		}
	}
	if err != nil {
		return nil, err
	}

	u, err := scan(tx.QueryRow(ctx, `
		WITH ids AS (SELECT 'upl_' || replace(gen_random_uuid()::text, '-', '') AS id)
		INSERT INTO uploads (id, user_id, artist_id, release_id, title, artist_name, album_title, genre, track_number,
		                     explicit, filename, size_bytes, original_key, rights_terms_version, rights_accepted_at)
		SELECT ids.id, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'originals/' || ids.id, $12, now() FROM ids
		RETURNING `+cols,
		n.UserID, artistID, releaseID, n.Title, artistName, releaseTitle, n.Genre, n.TrackNumber, n.Explicit,
		n.Filename, n.SizeBytes, n.TermsVersion))
	if err != nil {
		return nil, err
	}
	return u, tx.Commit(ctx)
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
// A failed upload no longer counts against the artist's storage.
func (s *Store) Fail(ctx context.Context, id, msg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE uploads SET status = 'failed', error = $2, locked_at = NULL, updated_at = now()
		WHERE id = $1 AND status IN ('pending','queued','processing')`, id, msg)
	return err
}

// Delete removes an upload that has not become a track (pending or failed) and
// returns its original's storage key. Finished uploads are removed by deleting
// the track.
func (s *Store) Delete(ctx context.Context, id, userID string) (string, error) {
	var key string
	err := s.pool.QueryRow(ctx, `
		DELETE FROM uploads WHERE id = $1 AND user_id = $2 AND status IN ('pending','failed')
		RETURNING original_key`, id, userID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if qerr := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM uploads WHERE id = $1 AND user_id = $2)`, id, userID).Scan(&exists); qerr != nil {
			return "", qerr
		}
		if exists {
			return "", ErrInUse
		}
		return "", ErrNotFound
	}
	return key, err
}

// ExpirePending fails uploads whose file never arrived and returns their
// storage keys so the caller can delete any partial object. This is what
// returns reserved quota from abandoned uploads.
func (s *Store) ExpirePending(ctx context.Context, olderThan time.Duration) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE uploads SET status = 'failed', error = 'the upload was never completed', updated_at = now()
		WHERE status = 'pending' AND created_at < now() - make_interval(secs => $1)
		RETURNING original_key`, olderThan.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
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

// ErrReleaseGone means the release the upload belonged to no longer exists.
var ErrReleaseGone = errors.New("upload: the release was deleted")

// Finish creates the track inside the upload's release, registers the
// renditions and marks the upload ready, all in one transaction. The track is
// public only when its release already is; otherwise publishing the release
// flips it.
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
	if u.ArtistID == "" || u.ReleaseID == "" {
		return ErrReleaseGone // uploaded before artist mode existed and never processed
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var releaseVisibility string
	err = tx.QueryRow(ctx, `SELECT visibility FROM albums WHERE id = $1 AND artist_id = $2 FOR SHARE`, u.ReleaseID, u.ArtistID).
		Scan(&releaseVisibility)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrReleaseGone
	}
	if err != nil {
		return err
	}
	trackVisibility := "draft"
	if releaseVisibility == "public" {
		trackVisibility = "public"
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO tracks (id, title, artist_id, album_id, duration_ms, explicit, genre, track_number, visibility)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		u.TrackID, u.Title, u.ArtistID, u.ReleaseID, res.DurationMs, u.Explicit, u.Genre, u.TrackNumber, trackVisibility); err != nil {
		return err
	}
	// Legacy single-file pointer: keeps the track visible to native search and
	// gives playback something to serve if renditions were ever lost.
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
	if _, err := tx.Exec(ctx, `UPDATE albums SET updated_at = now() WHERE id = $1`, u.ReleaseID); err != nil {
		return err
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
