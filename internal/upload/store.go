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
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/media"
	"github.com/blacmusic/backend/internal/review"
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

	// HeartbeatEvery is how often a worker says it is still on the upload it is processing.
	HeartbeatEvery = 30 * time.Second
	// StuckAfter without that signal means no worker is on the upload any more: it is stuck.
	StuckAfter = 3 * time.Minute
)

var (
	ErrNotFound        = errors.New("upload: not found")
	ErrLimit           = errors.New("upload: too many unfinished uploads")
	ErrDailyLimit      = errors.New("upload: daily upload limit reached")
	ErrNoArtist        = errors.New("upload: this account has no artist profile")
	ErrArtistBanned    = errors.New("upload: this artist profile is suspended")
	ErrReleaseNotFound = errors.New("upload: release not found")
	ErrInUse           = errors.New("upload: cannot be removed in its current state")
	ErrTrackNotFound   = errors.New("upload: track not found")
	ErrTrackNotDraft   = errors.New("upload: only a track in a draft release can have its file replaced")
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
	MultipartID string // open object-store upload id; empty for single-PUT uploads and once completed
	PartSize    int64  // 0 for single-PUT uploads
	Status      Status
	Error       string
	Attempts    int
	ClaimedAt   *time.Time // when a worker picked it up; nil unless it is being processed
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Store is the database side of uploads and the processing queue.
type Store struct {
	pool *pgxpool.Pool
	// OnReady, when set, is told (inside the transaction that finishes the last unfinished upload of a
	// release) that the release's audio is all processed. The worker sets it.
	OnReady ReadyHook
	// Review: a song an artist adds to an online release stays hidden until the team approves it.
	Review bool
}

// ReadyEvent says that every file uploaded to a release has been processed.
type ReadyEvent struct {
	UploadID     string // the upload that finished last
	ReleaseID    string
	ReleaseTitle string
	OwnerUserID  string
	Tracks       int  // tracks in the release now
	Live         bool // the release is already public
	Held         bool // the new songs wait for the team's review
}

// ReadyHook receives a ReadyEvent inside the finishing transaction. A failing hook is logged and
// rolled back on its own; it never stops a track from being finished.
type ReadyHook func(ctx context.Context, tx pgx.Tx, e ReadyEvent) error

// NewStore creates the store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const cols = `id, COALESCE(user_id::text, ''), COALESCE(artist_id, ''), COALESCE(release_id, ''), track_id, title, artist_name, album_title,
	genre, track_number, explicit, filename, size_bytes, original_key, multipart_id, part_size, status, COALESCE(error, ''),
	attempts, claimed_at, created_at, updated_at`

func scan(row pgx.Row) (*Upload, error) {
	var u Upload
	var st string
	err := row.Scan(&u.ID, &u.UserID, &u.ArtistID, &u.ReleaseID, &u.TrackID, &u.Title, &u.ArtistName, &u.AlbumTitle,
		&u.Genre, &u.TrackNumber, &u.Explicit, &u.Filename, &u.SizeBytes, &u.OriginalKey, &u.MultipartID, &u.PartSize, &st, &u.Error, &u.Attempts,
		&u.ClaimedAt, &u.CreatedAt, &u.UpdatedAt)
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
	// AsAdmin: an administrator uploads on behalf of the release artist (ReleaseID is required). No per-artist
	// upload limits apply, and no storage quota when the artist has no owner account.
	AsAdmin  bool
	PartSize int64 // > 0 makes this a resumable multipart upload with parts of this size
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
	var verified, banned, owned bool
	if n.AsAdmin {
		err = tx.QueryRow(ctx, `
			SELECT a.id, a.name, a.verification = 'verified', a.banned_at IS NOT NULL, a.owner_user_id IS NOT NULL
			FROM albums al JOIN artists a ON a.id = al.artist_id WHERE al.id = $1 FOR UPDATE OF a`, n.ReleaseID).
			Scan(&artistID, &artistName, &verified, &banned, &owned)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrReleaseNotFound
		}
	} else {
		owned = true
		err = tx.QueryRow(ctx, `SELECT id, name, verification = 'verified', banned_at IS NOT NULL FROM artists WHERE owner_user_id = $1 FOR UPDATE`, n.UserID).
			Scan(&artistID, &artistName, &verified, &banned)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoArtist
		}
	}
	if err != nil {
		return nil, err
	}
	if banned {
		return nil, ErrArtistBanned
	}

	if !n.AsAdmin {
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
	}

	if owned { // an artist without an account has no storage allowance to charge
		q, err := artist.QuotaFor(ctx, tx, artistID)
		if err != nil {
			return nil, err
		}
		if n.SizeBytes > q.RemainingStorageBytes {
			return nil, &QuotaError{Required: n.SizeBytes, Remaining: q.RemainingStorageBytes, Limit: q.TotalStorageBytes, Used: q.UsedStorageBytes}
		}
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
	if !n.AsAdmin {
		if err := review.Locked(ctx, tx, releaseID); err != nil {
			return nil, err
		}
	}

	u, err := scan(tx.QueryRow(ctx, `
		WITH ids AS (SELECT 'upl_' || replace(gen_random_uuid()::text, '-', '') AS id)
		INSERT INTO uploads (id, user_id, artist_id, release_id, title, artist_name, album_title, genre, track_number,
		                     explicit, filename, size_bytes, original_key, rights_terms_version, rights_accepted_at, part_size, by_team, quota_exempt)
		SELECT ids.id, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'originals/' || ids.id, $12, now(), $13, $14, $15 FROM ids
		RETURNING `+cols,
		n.UserID, artistID, releaseID, n.Title, artistName, releaseTitle, n.Genre, n.TrackNumber, n.Explicit,
		n.Filename, n.SizeBytes, n.TermsVersion, n.PartSize, n.AsAdmin, !owned)) // no account, no allowance to use
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

// SetMultipart records the object store's upload id for a multipart upload.
func (s *Store) SetMultipart(ctx context.Context, id, uploadID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE uploads SET multipart_id = $2 WHERE id = $1 AND status = 'pending'`, id, uploadID)
	return err
}

// Queue moves a pending upload to queued and records the size the file really
// has (never more than was declared, so the reserved storage can only shrink).
// Calling it again on an upload that is already past pending is a no-op that
// returns the current state.
func (s *Store) Queue(ctx context.Context, id string, actualSize int64) (*Upload, error) {
	u, err := scan(s.pool.QueryRow(ctx, `
		UPDATE uploads SET status = 'queued', size_bytes = LEAST(size_bytes, $2), multipart_id = '', updated_at = now()
		WHERE id = $1 AND status = 'pending' RETURNING `+cols, id, actualSize))
	if errors.Is(err, ErrNotFound) {
		return s.Get(ctx, id)
	}
	return u, err
}

// QueueStored queues a pending upload whose file is already in storage under key (a bulk import sends all its
// files before any upload exists). It runs inside the caller's transaction.
func (s *Store) QueueStored(ctx context.Context, tx pgx.Tx, id, key string) error {
	tag, err := tx.Exec(ctx, `UPDATE uploads SET original_key = $2, status = 'queued', updated_at = now() WHERE id = $1 AND status = 'pending'`, id, key)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// Fail marks an upload failed with a message that is safe to show the user.
// A failed upload no longer counts against the artist's storage.
func (s *Store) Fail(ctx context.Context, id, msg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE uploads SET status = 'failed', error = $2, locked_at = NULL, updated_at = now()
		WHERE id = $1 AND status IN ('pending','queued','processing')`, id, msg)
	return err
}

// Stale names stored data that must be deleted once its upload is gone: the
// object, and any unfinished multipart upload (whose parts also use storage).
type Stale struct {
	Key         string
	MultipartID string
}

// Delete removes an upload that has not become a track (pending or failed) and
// returns what to delete from storage. Finished uploads are removed by
// deleting the track.
func (s *Store) Delete(ctx context.Context, id, userID string) (*Stale, error) {
	var st Stale
	err := s.pool.QueryRow(ctx, `
		DELETE FROM uploads WHERE id = $1 AND user_id = $2 AND status IN ('pending','failed')
		RETURNING original_key, multipart_id`, id, userID).Scan(&st.Key, &st.MultipartID)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if qerr := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM uploads WHERE id = $1 AND user_id = $2)`, id, userID).Scan(&exists); qerr != nil {
			return nil, qerr
		}
		if exists {
			return nil, ErrInUse
		}
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// Discard removes an upload that no worker has started on (pending, queued or failed), for example
// when an artist swaps the file of a song in a draft. It returns what to delete from storage. A
// finished upload is removed by deleting its track; one being processed returns ErrInUse.
func (s *Store) Discard(ctx context.Context, id, userID string) (*Stale, error) {
	var st Stale
	err := s.pool.QueryRow(ctx, `
		DELETE FROM uploads WHERE id = $1 AND user_id = $2 AND status IN ('pending','queued','failed')
		RETURNING original_key, multipart_id`, id, userID).Scan(&st.Key, &st.MultipartID)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if qerr := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM uploads WHERE id = $1 AND user_id = $2)`, id, userID).Scan(&exists); qerr != nil {
			return nil, qerr
		}
		if exists {
			return nil, ErrInUse
		}
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// DeleteAny is Delete for an administrator: any user's upload that has not become a track.
func (s *Store) DeleteAny(ctx context.Context, id string) (*Stale, error) {
	var st Stale
	err := s.pool.QueryRow(ctx, `
		DELETE FROM uploads WHERE id = $1 AND status IN ('pending','failed')
		RETURNING original_key, multipart_id`, id).Scan(&st.Key, &st.MultipartID)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if qerr := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM uploads WHERE id = $1)`, id).Scan(&exists); qerr != nil {
			return nil, qerr
		}
		if exists {
			return nil, ErrInUse
		}
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// ExpirePending fails uploads whose file never arrived and returns what the
// caller must delete from storage (a partial object, or the parts of an
// unfinished multipart upload). This is what returns reserved quota from
// abandoned uploads.
func (s *Store) ExpirePending(ctx context.Context, olderThan time.Duration) ([]Stale, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE uploads SET status = 'failed', error = 'the upload was never completed', updated_at = now()
		WHERE status = 'pending' AND created_at < now() - make_interval(secs => $1)
		RETURNING original_key, multipart_id`, olderThan.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Stale
	for rows.Next() {
		var st Stale
		if err := rows.Scan(&st.Key, &st.MultipartID); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
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
		UPDATE uploads SET status = 'processing', attempts = attempts + 1, locked_at = now(), claimed_at = now(), updated_at = now()
		WHERE id = (
			SELECT id FROM uploads
			WHERE (status = 'queued'
			       -- an upload for a release that a transfer is filling waits until its transfer is being worked on
			       -- (matched by release, so it holds even before the artist has attached the upload)
			       AND NOT EXISTS (SELECT 1 FROM transfer_items ti JOIN transfers tr ON tr.id = ti.transfer_id
			                       WHERE ti.release_id = uploads.release_id AND ti.status NOT IN ('dropped', 'skipped_duplicate')
			                         AND tr.status IN ('collecting', 'queued', 'retrying')))
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

// Heartbeat says that the worker which claimed u is still on it. It returns false once the job is no longer that
// worker's: it finished, failed, was removed, or was restarted.
func (s *Store) Heartbeat(ctx context.Context, u *Upload) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE uploads SET locked_at = now()
		WHERE id = $1 AND status = 'processing' AND claimed_at IS NOT DISTINCT FROM $2`, u.ID, u.ClaimedAt)
	return tag.RowsAffected() == 1, err
}

// TakenOver says whether the job was restarted since u was claimed, so the worker holding u must not touch it or
// its file: it is waiting again, another worker has it, or another worker already finished it.
func (s *Store) TakenOver(ctx context.Context, u *Upload) (bool, error) {
	var over bool
	err := s.pool.QueryRow(ctx, `
		SELECT status IN ('queued', 'ready') OR (status = 'processing' AND claimed_at IS DISTINCT FROM $2)
		FROM uploads WHERE id = $1`, u.ID, u.ClaimedAt).Scan(&over)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return over, err
}

// Restart queues an upload again with all its tries, inside the caller's transaction. The caller has locked the
// row and decided that restarting it is right.
func Restart(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `
		UPDATE uploads SET status = 'queued', attempts = 0, error = NULL, locked_at = NULL, claimed_at = NULL, updated_at = now()
		WHERE id = $1`, id)
	return err
}

// Requeue puts a job back after a transient failure.
func (s *Store) Requeue(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE uploads SET status = 'queued', locked_at = NULL, claimed_at = NULL, updated_at = now()
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
	Renditions []Rendition     // must include "medium"
	Loudness   *media.Loudness // nil when it could not be measured (or the track is silent)
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
	err = tx.QueryRow(ctx, `SELECT visibility FROM albums WHERE id = $1 AND artist_id = $2 FOR UPDATE`, u.ReleaseID, u.ArtistID).
		Scan(&releaseVisibility)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrReleaseGone
	}
	if err != nil {
		return err
	}
	hold := false
	if s.Review && releaseVisibility != "draft" {
		if err := tx.QueryRow(ctx, `SELECT NOT by_team FROM uploads WHERE id = $1`, u.ID).Scan(&hold); err != nil {
			return err
		}
	}
	trackVisibility := "draft"
	if releaseVisibility == "public" && !hold {
		trackVisibility = "public"
	}

	var lufs, peak, lra *float64
	if res.Loudness != nil {
		lufs, peak, lra = &res.Loudness.IntegratedLUFS, &res.Loudness.TruePeakDB, &res.Loudness.RangeLU
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO tracks (id, title, artist_id, album_id, duration_ms, explicit, genre, track_number, visibility,
		                    loudness_lufs, true_peak_db, loudness_range_lu, loudness_checked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now())`,
		u.TrackID, u.Title, u.ArtistID, u.ReleaseID, res.DurationMs, u.Explicit, u.Genre, u.TrackNumber, trackVisibility,
		lufs, peak, lra); err != nil {
		return err
	}
	if hold {
		if _, err := tx.Exec(ctx, `UPDATE tracks SET review_hold = true WHERE id = $1`, u.TrackID); err != nil {
			return err
		}
		if err := review.Stage(ctx, tx, u.ReleaseID, func(c *review.Changes) { c.NewTracks = append(c.NewTracks, u.TrackID) }); err != nil {
			return err
		}
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
		WHERE id = $1 AND status = 'processing' AND claimed_at IS NOT DISTINCT FROM $2`, u.ID, u.ClaimedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("upload %s is no longer being processed", u.ID)
	}
	if s.OnReady != nil {
		s.notifyReady(ctx, tx, u, releaseVisibility == "public" && !hold, hold)
	}
	return tx.Commit(ctx)
}

// notifyReady calls OnReady when this was the last unfinished upload of a release that belongs to no
// transfer (transfers report on their own).
func (s *Store) notifyReady(ctx context.Context, tx pgx.Tx, u *Upload, live, held bool) {
	ev := ReadyEvent{UploadID: u.ID, ReleaseID: u.ReleaseID, Live: live, Held: held}
	err := tx.QueryRow(ctx, `
		SELECT al.title, a.owner_user_id::text, (SELECT count(*) FROM tracks WHERE album_id = al.id)
		FROM albums al JOIN artists a ON a.id = al.artist_id
		WHERE al.id = $1 AND a.owner_user_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM uploads o WHERE o.release_id = al.id AND o.id <> $2 AND o.status IN ('pending', 'queued', 'processing'))
		  AND NOT EXISTS (SELECT 1 FROM transfer_items ti WHERE ti.release_id = al.id)`, u.ReleaseID, u.ID).
		Scan(&ev.ReleaseTitle, &ev.OwnerUserID, &ev.Tracks)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("release ready lookup", "release", u.ReleaseID, "err", err)
		}
		return
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		slog.Error("release ready hook", "release", u.ReleaseID, "err", err)
		return
	}
	if herr := s.OnReady(ctx, sp, ev); herr != nil {
		slog.Error("release ready hook", "release", u.ReleaseID, "err", herr)
		_ = sp.Rollback(ctx)
		return
	}
	if err := sp.Commit(ctx); err != nil {
		slog.Error("release ready hook", "release", u.ReleaseID, "err", err)
	}
}

// TrackMeta is what a replacement upload inherits from the track it replaces.
type TrackMeta struct {
	ReleaseID   string
	Title       string
	Genre       string
	TrackNumber *int
	Explicit    bool
}

// DraftTrack loads a track of the user's that is still in a draft release.
func (s *Store) DraftTrack(ctx context.Context, trackID, userID string) (*TrackMeta, error) {
	var m TrackMeta
	var visibility string
	var genre *string
	err := s.pool.QueryRow(ctx, `
		SELECT t.album_id, t.title, t.genre, t.track_number, t.explicit, t.visibility
		FROM tracks t JOIN artists a ON a.id = t.artist_id
		WHERE t.id = $1 AND a.owner_user_id = $2 AND t.album_id IS NOT NULL`, trackID, userID).
		Scan(&m.ReleaseID, &m.Title, &genre, &m.TrackNumber, &m.Explicit, &visibility)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTrackNotFound
	}
	if err != nil {
		return nil, err
	}
	if visibility != "draft" {
		return nil, ErrTrackNotDraft
	}
	if genre != nil {
		m.Genre = *genre
	}
	return &m, nil
}
