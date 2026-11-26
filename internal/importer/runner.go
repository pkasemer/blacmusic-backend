package importer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/upload"
)

const (
	// maxInFlight is how many files of imports may wait for or be in the audio processor at once. The processor
	// takes the oldest upload first, so keeping this small lets an artist who uploads during a big import go ahead
	// of the songs that are not queued yet.
	maxInFlight = 2
	// maxAttempts is how often a step is retried after an error that is not the file's fault.
	maxAttempts = 5
	// filesTTL is how long a job waits for its files before it is cancelled.
	filesTTL = 24 * time.Hour
)

// RunnerBlobs is the object storage the runner needs.
type RunnerBlobs interface {
	image.Blobs
	AbortMultipart(ctx context.Context, key, uploadID string) error
}

// Runner does the work of started jobs. It runs in the worker. Every step is recorded in the database as it
// happens, so a job carries on after a restart.
type Runner struct {
	pool     *pgxpool.Pool
	blobs    RunnerBlobs
	releases *release.Store // without publish hooks: an import sends no "your release is live" mail
	uploads  *upload.Store
	// Interval is the pause between two rounds.
	Interval time.Duration
}

// NewRunner creates the runner. releases must have no publish hooks set.
func NewRunner(pool *pgxpool.Pool, blobs RunnerBlobs, releases *release.Store, uploads *upload.Store) *Runner {
	return &Runner{pool: pool, blobs: blobs, releases: releases, uploads: uploads, Interval: 5 * time.Second}
}

// Run works until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	for {
		if err := r.Step(ctx); err != nil && ctx.Err() == nil {
			slog.Error("imports", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.Interval):
		}
	}
}

// Step moves every job forward as far as it can go right now.
func (r *Runner) Step(ctx context.Context) error {
	if err := r.expire(ctx); err != nil {
		return err
	}
	ids, err := r.column(ctx, `SELECT id FROM import_jobs WHERE status = 'processing' ORDER BY started_at, id`)
	if err != nil {
		return err
	}
	var first error
	for _, id := range ids {
		if err := r.locked(ctx, id); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (r *Runner) column(ctx context.Context, sql string, args ...any) ([]string, error) {
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// expire cancels jobs whose files never came.
func (r *Runner) expire(ctx context.Context) error {
	ids, err := r.column(ctx, `
		UPDATE import_jobs SET status = 'cancelled', finished_at = now(), error = 'the files did not all arrive within 24 hours'
		WHERE status = 'awaiting_files' AND created_at < now() - make_interval(secs => $1) RETURNING id`, filesTTL.Seconds())
	if err != nil {
		return err
	}
	for _, id := range ids {
		files, err := jobFiles(ctx, r.pool, id)
		if err != nil {
			return err
		}
		removeStaged(ctx, r.blobs, files)
	}
	return nil
}

// locked runs one job while holding its lock, so two workers never work on the same job.
func (r *Runner) locked(ctx context.Context, id string) error {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 24))`, id).Scan(&got); err != nil || !got {
		return err
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1, 24))`, id); err != nil {
			_ = conn.Conn().Close(context.WithoutCancel(ctx)) // a closed connection gives up its locks
		}
	}()
	return r.job(ctx, id)
}

type job struct {
	id, createdBy, apiKeyID, apiKeyName, creatorEmail string
	artistID, artistName, artistBio, pictureFile      string
	releaseType, publish                              string
	albumTitle, albumGenre, albumCover, albumDate     string
	albumReleaseID                                    string
}

type item struct {
	position                                                int
	title, genre, releaseDate, audioFile, coverFile, status string
	releaseID, uploadID                                     string
	explicit                                                bool
	attempts                                                int
}

// itemError is a failure that retrying cannot fix.
type itemError struct{ code, msg string }

func (e *itemError) Error() string { return e.msg }

// permanent turns an error into a reason that retrying cannot fix, or nil when it may pass by itself.
func permanent(err error) *itemError {
	var ie *itemError
	var ve *release.ValidationError
	var bad *image.InvalidImageError
	switch {
	case errors.As(err, &ie):
		return ie
	case errors.As(err, &bad):
		return &itemError{"invalid_cover", bad.Msg}
	case errors.As(err, &ve):
		return &itemError{"invalid_release", ve.Error()}
	case errors.Is(err, release.ErrArtistBanned), errors.Is(err, upload.ErrArtistBanned):
		return &itemError{"artist_banned", "the artist is banned"}
	case errors.Is(err, release.ErrNotFound), errors.Is(err, upload.ErrReleaseNotFound):
		return &itemError{"removed", "the artist or the release was removed"}
	case errors.Is(err, release.ErrTakenDown):
		return &itemError{"taken_down", "the release was taken down"}
	case errors.Is(err, release.ErrNoTracks):
		return &itemError{"no_tracks", "the release has no finished track"}
	}
	var quota *upload.QuotaError
	if errors.As(err, &quota) {
		return &itemError{"quota_exceeded", "the artist does not have enough storage left"}
	}
	return nil
}

// audioError names what the audio processor said about a file.
func audioError(msg string) *itemError {
	code := "processing_failed"
	switch {
	case strings.HasPrefix(msg, "unsupported file type"), msg == "the file has no audio":
		code = "unsupported_audio"
	case strings.HasPrefix(msg, "too long"):
		code = "file_too_long"
	case strings.HasPrefix(msg, "could not read"), strings.HasPrefix(msg, "could not convert"):
		code = "corrupt_audio"
	case strings.HasPrefix(msg, "the uploaded file is missing"):
		code = "missing_file"
	}
	return &itemError{code, msg}
}

func (r *Runner) load(ctx context.Context, id string) (*job, error) {
	j := &job{id: id}
	err := r.pool.QueryRow(ctx, `
		SELECT j.created_by::text, u.email, COALESCE(k.id, ''), COALESCE(k.name, ''), COALESCE(j.artist_id, ''), j.artist_name, j.artist_bio,
		       j.picture_file, j.release_type, j.publish, j.album_title, j.album_genre, j.album_cover_file, j.album_date, j.album_release_id
		FROM import_jobs j JOIN users u ON u.id = j.created_by LEFT JOIN api_keys k ON k.id = j.api_key_id
		WHERE j.id = $1 AND j.status = 'processing'`, id).
		Scan(&j.createdBy, &j.creatorEmail, &j.apiKeyID, &j.apiKeyName, &j.artistID, &j.artistName, &j.artistBio,
			&j.pictureFile, &j.releaseType, &j.publish, &j.albumTitle, &j.albumGenre, &j.albumCover, &j.albumDate, &j.albumReleaseID)
	return j, err
}

func (r *Runner) items(ctx context.Context, id string, statuses ...string) ([]item, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT position, title, genre, release_date, audio_file, cover_file, status, release_id, upload_id, explicit, attempts
		FROM import_items WHERE job_id = $1 AND status = ANY($2) ORDER BY position`, id, statuses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.position, &it.title, &it.genre, &it.releaseDate, &it.audioFile, &it.coverFile, &it.status, &it.releaseID, &it.uploadID, &it.explicit, &it.attempts); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// audit writes an audit row in the name of the administrator who created the job.
func (r *Runner) audit(ctx context.Context, tx pgx.Tx, j *job, action, targetType, targetID, label string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	details["importId"] = j.id
	if j.apiKeyID != "" {
		details["apiKeyId"], details["apiKeyName"] = j.apiKeyID, j.apiKeyName
	}
	return audit.Write(ctx, tx, audit.Entry{ActorID: j.createdBy, ActorEmail: j.creatorEmail, Action: action, TargetType: targetType, TargetID: targetID,
		TargetLabel: label, Details: details})
}

func (r *Runner) job(ctx context.Context, id string) error {
	j, err := r.load(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	// first collect what the audio processor has finished
	running, err := r.items(ctx, id, "processing")
	if err != nil {
		return err
	}
	for _, it := range running {
		if err := r.collect(ctx, j, it); err != nil {
			if err := r.stumble(ctx, j, it, err); err != nil {
				return err
			}
		}
	}

	// then start more
	waiting, err := r.items(ctx, id, "pending")
	if err != nil {
		return err
	}
	if len(waiting) > 0 {
		if err := r.ensureArtist(ctx, j); err != nil {
			if p := permanent(err); p != nil {
				return r.abort(ctx, j, p)
			}
			return err
		}
		var inFlight int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM import_items i JOIN uploads u ON u.id = i.upload_id
			WHERE i.status = 'processing' AND u.status IN ('queued', 'processing')`).Scan(&inFlight); err != nil {
			return err
		}
		for _, it := range waiting {
			if inFlight >= maxInFlight {
				break
			}
			started, err := r.begin(ctx, j, it)
			if err != nil {
				if err := r.stumble(ctx, j, it, err); err != nil {
					return err
				}
				continue
			}
			if started {
				inFlight++
			}
		}
	}
	return r.finish(ctx, j)
}

// stumble records an error on one item: for good when retrying cannot help or has been tried often enough.
func (r *Runner) stumble(ctx context.Context, j *job, it item, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	p := permanent(cause)
	if p == nil {
		slog.Warn("import step failed, will retry", "import", j.id, "item", it.position, "attempt", it.attempts+1, "err", cause)
		if it.attempts+1 < maxAttempts {
			_, err := r.pool.Exec(ctx, `UPDATE import_items SET attempts = attempts + 1 WHERE job_id = $1 AND position = $2`, j.id, it.position)
			return err
		}
		p = &itemError{"internal_error", "this song could not be imported because of a server error: send it again in a new import"}
	}
	return r.fail(ctx, j, it.position, p)
}

// fail marks an item failed and removes what was made for it.
func (r *Runner) fail(ctx context.Context, j *job, position int, why *itemError) error {
	var releaseID, uploadID string
	err := r.pool.QueryRow(ctx, `
		UPDATE import_items SET status = 'failed', error_code = $3, error_message = $4
		WHERE job_id = $1 AND position = $2 AND status IN ('pending', 'processing') RETURNING release_id, upload_id`,
		j.id, position, why.code, why.msg).Scan(&releaseID, &uploadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if uploadID != "" { // an upload that did not become a track
		if stale, err := r.uploads.DeleteAny(ctx, uploadID); err == nil {
			_ = r.blobs.Remove(ctx, stale.Key)
		}
	}
	if j.releaseType == "single" && releaseID != "" {
		removed, err := r.removeEmptyDraft(ctx, j, releaseID)
		if err != nil {
			slog.Warn("import: empty draft not removed", "import", j.id, "release", releaseID, "err", err)
		}
		if removed {
			if _, err := r.pool.Exec(ctx, `UPDATE import_items SET release_id = '' WHERE job_id = $1 AND position = $2`, j.id, position); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeEmptyDraft deletes a release the import made that never got a track.
func (r *Runner) removeEmptyDraft(ctx context.Context, j *job, releaseID string) (bool, error) {
	var empty bool
	var title string
	err := r.pool.QueryRow(ctx, `SELECT title, visibility = 'draft' AND NOT EXISTS (SELECT 1 FROM tracks WHERE album_id = al.id) FROM albums al WHERE id = $1`, releaseID).Scan(&title, &empty)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil || !empty {
		return false, err
	}
	err = r.releases.AdminDelete(ctx, releaseID, func(ctx context.Context, tx pgx.Tx) error {
		return r.audit(ctx, tx, j, "release.delete", "release", releaseID, title, map[string]any{"reason": "the import of its song failed"})
	})
	return err == nil, err
}

// abort ends a job that cannot go on.
func (r *Runner) abort(ctx context.Context, j *job, why *itemError) error {
	left, err := r.items(ctx, j.id, "pending")
	if err != nil {
		return err
	}
	for _, it := range left {
		if err := r.fail(ctx, j, it.position, why); err != nil {
			return err
		}
	}
	return r.finish(ctx, j)
}

// ensureArtist finds or creates the artist of the job.
func (r *Runner) ensureArtist(ctx context.Context, j *job) error {
	if j.artistID != "" {
		return nil
	}
	found, status, code, err := findArtist(ctx, r.pool, ArtistSpec{Name: j.artistName})
	if err != nil {
		if status != 0 {
			return &itemError{code, err.Error()}
		}
		return err
	}
	created := false
	if found.id == "" {
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		// two imports of the same new artist must not create it twice
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('import-artist:' || lower($1), 24))`, j.artistName); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT id FROM artists WHERE lower(btrim(name)) = lower($1) AND owner_user_id IS NULL AND banned_at IS NULL ORDER BY created_at, id LIMIT 1`, j.artistName).Scan(&found.id)
		if errors.Is(err, pgx.ErrNoRows) {
			if err := tx.QueryRow(ctx, `
				INSERT INTO artists (name, bio, owner_user_id, flagged)
				VALUES ($1, $2, NULL, EXISTS (SELECT 1 FROM artists WHERE verification = 'verified' AND lower(name) = lower($1)))
				RETURNING id`, j.artistName, j.artistBio).Scan(&found.id); err != nil {
				return err
			}
			created = true
			if err := r.audit(ctx, tx, j, "artist.create", "artist", found.id, j.artistName, map[string]any{"owner": nil}); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE import_jobs SET artist_id = $2 WHERE id = $1`, j.id, found.id); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	} else if _, err := r.pool.Exec(ctx, `UPDATE import_jobs SET artist_id = $2 WHERE id = $1`, j.id, found.id); err != nil {
		return err
	}
	j.artistID = found.id
	if created && j.pictureFile != "" {
		// the picture is a nicety: a bad one never stops the songs
		id, err := r.picture(ctx, j, j.pictureFile, "avatar")
		if err == nil {
			_, err = r.pool.Exec(ctx, `UPDATE artists SET avatar_image_id = $2 WHERE id = $1`, j.artistID, id)
		}
		if err != nil {
			reason := "it could not be saved"
			var bad *image.InvalidImageError
			if errors.As(err, &bad) {
				reason = bad.Msg
			}
			slog.Warn("import: artist picture not used", "import", j.id, "err", err)
			if err := r.warn(ctx, j.id, "The artist picture was not used: "+reason+". Add it on the artist page."); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Runner) warn(ctx context.Context, jobID, msg string) error {
	_, err := r.pool.Exec(ctx, `UPDATE import_jobs SET warnings = array_append(warnings, $2) WHERE id = $1`, jobID, msg)
	return err
}

// picture turns a staged image file into a finished image of the administrator, once per file.
func (r *Runner) picture(ctx context.Context, j *job, name, purpose string) (string, error) {
	var key, imageID string
	err := r.pool.QueryRow(ctx, `SELECT storage_key, image_id FROM import_files WHERE job_id = $1 AND name = $2`, j.id, name).Scan(&key, &imageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", &itemError{"missing_file", "the picture was not sent"}
	}
	if err != nil {
		return "", err
	}
	if imageID != "" { // reuse it, unless it was cleared away or made for something else
		var ok bool
		if err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM images WHERE id = $1 AND purpose = $2 AND status = 'ready')`, imageID, purpose).Scan(&ok); err != nil {
			return "", err
		}
		if ok {
			return imageID, nil
		}
	}
	rc, _, err := r.blobs.Open(ctx, key)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, image.MaxUploadBytes+1))
	if err != nil {
		return "", err
	}
	if len(raw) > image.MaxUploadBytes {
		return "", &image.InvalidImageError{Msg: "an image is at most 10 MB"}
	}
	if imageID, err = image.Ingest(ctx, r.pool, r.blobs, j.createdBy, purpose, raw); err != nil {
		return "", err
	}
	_, err = r.pool.Exec(ctx, `UPDATE import_files SET image_id = $3 WHERE job_id = $1 AND name = $2`, j.id, name, imageID)
	return imageID, err
}

// begin creates the release of one waiting item (if it has none yet) and hands its audio to the processor.
// started is false when the item turned out to be a duplicate.
func (r *Runner) begin(ctx context.Context, j *job, it item) (started bool, err error) {
	releaseID, started, err := r.releaseFor(ctx, j, it)
	if err != nil || !started {
		return false, err
	}
	var key string
	var size int64
	if err := r.pool.QueryRow(ctx, `SELECT storage_key, actual_bytes FROM import_files WHERE job_id = $1 AND name = $2`, j.id, it.audioFile).Scan(&key, &size); err != nil {
		return false, err
	}
	filename := it.audioFile
	if utf8.RuneCountInString(filename) > 200 {
		filename = string([]rune(filename)[utf8.RuneCountInString(filename)-200:])
	}
	// an upload left over from a try that was interrupted would keep the release from being published
	if _, err := r.pool.Exec(ctx, `DELETE FROM uploads WHERE release_id = $1 AND user_id::text = $2 AND status = 'pending' AND filename = $3 AND rights_terms_version = 'admin'`,
		releaseID, j.createdBy, filename); err != nil {
		return false, err
	}
	var number *int
	if j.releaseType == "album" {
		n := it.position
		number = &n
	}
	u, err := r.uploads.Create(ctx, upload.NewUpload{
		AsAdmin: true, UserID: j.createdBy, ReleaseID: releaseID, Title: it.title, Genre: it.genre, TrackNumber: number,
		Explicit: it.explicit, Filename: filename, SizeBytes: size, TermsVersion: "admin",
	})
	if err != nil {
		return false, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := r.uploads.QueueStored(ctx, tx, u.ID, key); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE import_items SET status = 'processing', upload_id = $3, attempts = 0 WHERE job_id = $1 AND position = $2`, j.id, it.position, u.ID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// releaseFor gives the release an item goes into, creating it if needed. ok is false when the item is a
// duplicate and has been marked so.
func (r *Runner) releaseFor(ctx context.Context, j *job, it item) (id string, ok bool, err error) {
	album := j.releaseType == "album"
	if album && j.albumReleaseID != "" {
		return j.albumReleaseID, true, nil
	}
	if !album && it.releaseID != "" {
		return it.releaseID, true, nil
	}
	title, genreName, coverFile, date, kind := it.title, it.genre, it.coverFile, it.releaseDate, "single"
	if album {
		title, genreName, coverFile, date, kind = j.albumTitle, j.albumGenre, j.albumCover, j.albumDate, "album"
	}
	// checked again here: it may have been added since the job was made
	have, err := existingReleases(ctx, r.pool, j.artistID)
	if err != nil {
		return "", false, err
	}
	if existing := have[normalizedTitle(title)]; existing != "" {
		where := `job_id = $1 AND position = $3`
		args := []any{j.id, existing, it.position}
		if album {
			where, args = `job_id = $1 AND status = 'pending'`, args[:2]
		}
		_, err := r.pool.Exec(ctx, `UPDATE import_items SET status = 'skippedDuplicate', release_id = $2 WHERE `+where, args...)
		return "", false, err
	}
	cover := ""
	if coverFile != "" {
		if cover, err = r.picture(ctx, j, coverFile, "cover"); err != nil {
			return "", false, err
		}
	}
	id, err = r.releases.AdminCreate(ctx, j.artistID, j.createdBy, release.Input{Type: kind, Title: title, Genre: genreName, ReleaseDate: date, CoverImageID: cover},
		func(ctx context.Context, tx pgx.Tx, id string) error {
			// the id is saved with the release itself, so a restart never makes a second one
			var err error
			if album {
				_, err = tx.Exec(ctx, `UPDATE import_jobs SET album_release_id = $2 WHERE id = $1`, j.id, id)
				if err == nil {
					_, err = tx.Exec(ctx, `UPDATE import_items SET release_id = $2 WHERE job_id = $1 AND status = 'pending'`, j.id, id)
				}
			} else {
				_, err = tx.Exec(ctx, `UPDATE import_items SET release_id = $3 WHERE job_id = $1 AND position = $2`, j.id, it.position, id)
			}
			if err != nil {
				return err
			}
			return r.audit(ctx, tx, j, "release.create", "release", id, title, map[string]any{"artistId": j.artistID, "artistName": j.artistName, "type": kind})
		})
	if err != nil {
		return "", false, err
	}
	if album {
		j.albumReleaseID = id
	}
	return id, true, nil
}

// collect looks at the upload of a processing item, and finishes the item when the processor is done with it.
func (r *Runner) collect(ctx context.Context, j *job, it item) error {
	var status, msg, trackID string
	err := r.pool.QueryRow(ctx, `SELECT status, COALESCE(error, ''), track_id FROM uploads WHERE id = $1`, it.uploadID).Scan(&status, &msg, &trackID)
	if errors.Is(err, pgx.ErrNoRows) {
		return &itemError{"removed", "the upload was removed while it was being imported"}
	}
	if err != nil {
		return err
	}
	switch upload.Status(status) {
	case upload.StatusFailed:
		return audioError(msg)
	case upload.StatusReady:
	default:
		return nil // still waiting
	}
	if j.releaseType == "single" && j.publish == "public" {
		if err := r.publish(ctx, j, it.releaseID); err != nil {
			if p := permanent(err); p != nil {
				p.code, p.msg = "publish_failed", "the song was added but left as a draft: "+p.msg
				return p
			}
			return err
		}
	}
	_, err = r.pool.Exec(ctx, `UPDATE import_items SET status = 'created', track_id = $3 WHERE job_id = $1 AND position = $2 AND status = 'processing'`, j.id, it.position, trackID)
	return err
}

func (r *Runner) publish(ctx context.Context, j *job, releaseID string) error {
	return r.releases.AdminPublish(ctx, releaseID, nil, func(ctx context.Context, tx pgx.Tx) error {
		var title string
		if err := tx.QueryRow(ctx, `SELECT title FROM albums WHERE id = $1`, releaseID).Scan(&title); err != nil {
			return err
		}
		return r.audit(ctx, tx, j, "release.publish", "release", releaseID, title, nil)
	})
}

// finish closes the job once nothing is waiting or processing.
func (r *Runner) finish(ctx context.Context, j *job) error {
	var open, created, skipped, failed int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status IN ('pending', 'processing')), count(*) FILTER (WHERE status = 'created'),
		count(*) FILTER (WHERE status = 'skippedDuplicate'), count(*) FILTER (WHERE status = 'failed') FROM import_items WHERE job_id = $1`, j.id).
		Scan(&open, &created, &skipped, &failed); err != nil {
		return err
	}
	if open > 0 {
		return nil
	}
	jobError := ""
	if j.releaseType == "album" && j.albumReleaseID != "" {
		switch {
		case created == 0:
			if _, err := r.removeEmptyDraft(ctx, j, j.albumReleaseID); err != nil {
				slog.Warn("import: empty album not removed", "import", j.id, "err", err)
			}
		case j.publish == "public" && failed > 0:
			// an album with missing songs is not put online by itself
			jobError = "The album was left as a draft because some songs failed. Add them or publish it as it is from the admin site."
		case j.publish == "public":
			if err := r.publish(ctx, j, j.albumReleaseID); err != nil {
				p := permanent(err)
				if p == nil {
					return err
				}
				jobError = "The album was left as a draft: " + p.msg
			}
		}
	}
	status := "completed"
	switch {
	case failed > 0 && created+skipped == 0:
		status = "failed"
	case failed > 0 || jobError != "":
		status = "completed_with_errors"
	}
	if _, err := r.pool.Exec(ctx, `UPDATE import_jobs SET status = $2, error = $3, finished_at = now() WHERE id = $1 AND status = 'processing'`, j.id, status, jobError); err != nil {
		return err
	}
	slog.Info("import finished", "import", j.id, "status", status, "created", created, "skipped", skipped, "failed", failed)

	// staged files that did not become the original of a song are not needed any more
	files, err := jobFiles(ctx, r.pool, j.id)
	if err != nil {
		return err
	}
	var unused []fileRow
	for _, f := range files {
		var used bool
		if err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM uploads WHERE original_key = $1)`, f.key).Scan(&used); err != nil {
			return err
		}
		if !used {
			unused = append(unused, f)
		}
	}
	removeStaged(ctx, r.blobs, unused)
	return nil
}
