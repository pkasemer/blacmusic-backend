// Package release implements releases (singles and albums), their draft /
// scheduled / public lifecycle, and editing or deleting the tracks in them.
package release

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/image"
)

var (
	ErrNotFound       = errors.New("release: not found")
	ErrNoArtist       = errors.New("release: this account has no artist profile")
	ErrUploadsPending = errors.New("release: some uploads are still being processed")
	ErrProcessing     = errors.New("release: a file is being processed right now")
)

// ValidationError is returned for bad user input.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Blobs removes objects from storage.
type Blobs interface {
	Remove(ctx context.Context, key string) error
}

// Store owns releases and the tracks inside them.
type Store struct {
	pool      *pgxpool.Pool
	blobs     Blobs
	publicURL string
}

// NewStore creates the store.
func NewStore(pool *pgxpool.Pool, blobs Blobs, publicURL string) *Store {
	return &Store{pool: pool, blobs: blobs, publicURL: publicURL}
}

// ArtistRef is the artist a release belongs to.
type ArtistRef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Verified bool   `json:"verified"`
}

// TrackView is a track inside a release.
type TrackView struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	TrackNumber *int   `json:"trackNumber,omitempty"`
	DurationMs  int    `json:"durationMs"`
	Explicit    bool   `json:"explicit"`
	Genre       string `json:"genre,omitempty"`
	Visibility  string `json:"visibility,omitempty"` // owner only
}

// UploadView is a file still on its way to becoming a track (owner only).
type UploadView struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	TrackNumber *int   `json:"trackNumber,omitempty"`
}

// Release is the API shape of a single or album.
type Release struct {
	ID          string     `json:"id"`
	Type        string     `json:"type"` // single | album
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	Genre       string     `json:"genre,omitempty"`
	ReleaseDate string     `json:"releaseDate,omitempty"` // YYYY-MM-DD
	ArtworkURL  string     `json:"artworkUrl,omitempty"`
	Visibility  string     `json:"visibility"` // draft | scheduled | public
	PublishAt   *time.Time `json:"publishAt,omitempty"`
	PublishedAt *time.Time `json:"publishedAt,omitempty"`
	Artist      *ArtistRef `json:"artist,omitempty"`
	TrackCount  int        `json:"trackCount"`
	Mine        bool       `json:"mine,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`

	Tracks  []TrackView  `json:"tracks,omitempty"`
	Uploads []UploadView `json:"uploads,omitempty"` // owner only: unfinished and failed files
}

const releaseSelect = `
	SELECT al.id, al.release_type, al.title, al.description, al.genre, COALESCE(to_char(al.release_date, 'YYYY-MM-DD'), ''),
	       COALESCE(al.artwork_url, ''), al.visibility, al.publish_at, al.published_at, al.created_at,
	       COALESCE(a.id, ''), COALESCE(a.name, ''), COALESCE(a.verification = 'verified', false), COALESCE(a.owner_user_id::text, ''),
	       (SELECT count(*) FROM tracks t WHERE t.album_id = al.id)
	FROM albums al LEFT JOIN artists a ON a.id = al.artist_id`

func scanRelease(row pgx.Row, viewer string) (*Release, error) {
	var r Release
	var ar ArtistRef
	var owner string
	err := row.Scan(&r.ID, &r.Type, &r.Title, &r.Description, &r.Genre, &r.ReleaseDate, &r.ArtworkURL, &r.Visibility,
		&r.PublishAt, &r.PublishedAt, &r.CreatedAt, &ar.ID, &ar.Name, &ar.Verified, &owner, &r.TrackCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if ar.ID != "" {
		r.Artist = &ar
	}
	r.Mine = viewer != "" && owner == viewer
	if !r.Mine {
		r.PublishAt = nil
	}
	return &r, nil
}

// Get returns a release. Drafts and scheduled releases are visible to their
// owner only; everyone else gets ErrNotFound.
func (s *Store) Get(ctx context.Context, id, viewer string) (*Release, error) {
	r, err := scanRelease(s.pool.QueryRow(ctx, releaseSelect+` WHERE al.id = $1`, id), viewer)
	if err != nil {
		return nil, err
	}
	if r.Visibility != "public" && !r.Mine {
		return nil, ErrNotFound
	}

	q := `SELECT t.id, t.title, t.track_number, t.duration_ms, t.explicit, t.genre, t.visibility FROM tracks t
	      WHERE t.album_id = $1`
	if !r.Mine {
		q += ` AND t.visibility = 'public'`
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY t.track_number NULLS LAST, t.created_at, t.id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t TrackView
		if err := rows.Scan(&t.ID, &t.Title, &t.TrackNumber, &t.DurationMs, &t.Explicit, &t.Genre, &t.Visibility); err != nil {
			rows.Close()
			return nil, err
		}
		if !r.Mine {
			t.Visibility = ""
		}
		r.Tracks = append(r.Tracks, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	r.TrackCount = len(r.Tracks)

	if r.Mine {
		urows, err := s.pool.Query(ctx, `SELECT id, title, status, COALESCE(error, ''), track_number FROM uploads
			WHERE release_id = $1 AND status <> 'ready' ORDER BY created_at, id`, id)
		if err != nil {
			return nil, err
		}
		defer urows.Close()
		for urows.Next() {
			var u UploadView
			if err := urows.Scan(&u.ID, &u.Title, &u.Status, &u.Error, &u.TrackNumber); err != nil {
				return nil, err
			}
			r.Uploads = append(r.Uploads, u)
		}
		if err := urows.Err(); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (s *Store) collect(rows pgx.Rows, viewer string) ([]*Release, error) {
	defer rows.Close()
	out := []*Release{}
	for rows.Next() {
		r, err := scanRelease(rows, viewer)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListMine returns every release of the user's artist profile, any visibility.
func (s *Store) ListMine(ctx context.Context, userID string, limit, offset int) ([]*Release, error) {
	rows, err := s.pool.Query(ctx, releaseSelect+` WHERE a.owner_user_id = $1
		ORDER BY al.updated_at DESC, al.id LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	return s.collect(rows, userID)
}

// ListByArtist returns an artist's public releases, newest first.
func (s *Store) ListByArtist(ctx context.Context, artistID, viewer string, limit, offset int) ([]*Release, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM artists WHERE id = $1)`, artistID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, releaseSelect+` WHERE al.artist_id = $1 AND al.visibility = 'public'
		ORDER BY al.release_date DESC NULLS LAST, al.published_at DESC, al.id LIMIT $2 OFFSET $3`, artistID, limit, offset)
	if err != nil {
		return nil, err
	}
	return s.collect(rows, viewer)
}

// ------------------------------------------------------------------- editing

// Input is the editable release metadata.
type Input struct {
	Type         string
	Title        string
	Description  string
	Genre        string
	ReleaseDate  string // YYYY-MM-DD or ""
	CoverImageID string // "" for none
}

func validate(in Input) (*time.Time, error) {
	if in.Type != "single" && in.Type != "album" {
		return nil, &ValidationError{"type must be single or album"}
	}
	if n := utf8.RuneCountInString(in.Title); n < 1 || n > 200 {
		return nil, &ValidationError{"title must be 1-200 characters"}
	}
	if utf8.RuneCountInString(in.Description) > 2000 {
		return nil, &ValidationError{"description must be at most 2000 characters"}
	}
	if utf8.RuneCountInString(in.Genre) > 40 {
		return nil, &ValidationError{"genre must be at most 40 characters"}
	}
	if in.ReleaseDate == "" {
		return nil, nil
	}
	d, err := time.Parse("2006-01-02", in.ReleaseDate)
	if err != nil {
		return nil, &ValidationError{"releaseDate must look like 2024-03-31"}
	}
	return &d, nil
}

// Create makes a draft release for the user's artist profile.
func (s *Store) Create(ctx context.Context, userID string, in Input) (*Release, error) {
	in.Title, in.Description, in.Genre = strings.TrimSpace(in.Title), strings.TrimSpace(in.Description), strings.TrimSpace(in.Genre)
	date, err := validate(in)
	if err != nil {
		return nil, err
	}
	artistID, err := artist.IDByOwner(ctx, s.pool, userID)
	if errors.Is(err, artist.ErrNotFound) {
		return nil, ErrNoArtist
	}
	if err != nil {
		return nil, err
	}
	var cover, coverURL *string
	if in.CoverImageID != "" {
		if err := image.CheckOwned(ctx, s.pool, in.CoverImageID, userID, "cover"); err != nil {
			return nil, &ValidationError{"coverImageId must be one of your finished cover images"}
		}
		u := image.URL(s.publicURL, in.CoverImageID, image.SizeMedium)
		cover, coverURL = &in.CoverImageID, &u
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO albums (title, artist_id, release_type, description, genre, release_date, cover_image_id, artwork_url, visibility)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'draft') RETURNING id`,
		in.Title, artistID, in.Type, in.Description, in.Genre, date, cover, coverURL).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id, userID)
}

// Patch holds the editable fields; nil means unchanged.
type Patch struct {
	Type         *string
	Title        *string
	Description  *string
	Genre        *string
	ReleaseDate  *string // "" clears
	CoverImageID *string // "" clears
}

// Update edits a release the user owns.
func (s *Store) Update(ctx context.Context, id, userID string, p Patch) (*Release, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var in Input
	var date *time.Time
	var cover *string
	var visibility string
	err = tx.QueryRow(ctx, `
		SELECT al.release_type, al.title, al.description, al.genre, al.release_date, al.cover_image_id, al.visibility
		FROM albums al JOIN artists a ON a.id = al.artist_id
		WHERE al.id = $1 AND a.owner_user_id = $2 FOR UPDATE OF al`, id, userID).
		Scan(&in.Type, &in.Title, &in.Description, &in.Genre, &date, &cover, &visibility)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if date != nil {
		in.ReleaseDate = date.Format("2006-01-02")
	}
	if cover != nil {
		in.CoverImageID = *cover
	}

	if p.Type != nil {
		if *p.Type != in.Type && visibility != "draft" {
			return nil, &ValidationError{"the type can only be changed while the release is a draft"}
		}
		in.Type = *p.Type
	}
	if p.Title != nil {
		in.Title = strings.TrimSpace(*p.Title)
	}
	if p.Description != nil {
		in.Description = strings.TrimSpace(*p.Description)
	}
	if p.Genre != nil {
		in.Genre = strings.TrimSpace(*p.Genre)
	}
	if p.ReleaseDate != nil {
		in.ReleaseDate = *p.ReleaseDate
	}
	coverChanged := false
	if p.CoverImageID != nil {
		coverChanged = true
		in.CoverImageID = *p.CoverImageID
	}
	newDate, err := validate(in)
	if err != nil {
		return nil, err
	}
	var coverID, coverURL *string
	if in.CoverImageID != "" {
		if coverChanged {
			if err := image.CheckOwned(ctx, tx, in.CoverImageID, userID, "cover"); err != nil {
				return nil, &ValidationError{"coverImageId must be one of your finished cover images"}
			}
		}
		u := image.URL(s.publicURL, in.CoverImageID, image.SizeMedium)
		coverID, coverURL = &in.CoverImageID, &u
	}
	if _, err := tx.Exec(ctx, `
		UPDATE albums SET release_type=$2, title=$3, description=$4, genre=$5, release_date=$6,
		       cover_image_id=$7, artwork_url=$8, updated_at=now() WHERE id=$1`,
		id, in.Type, in.Title, in.Description, in.Genre, newDate, coverID, coverURL); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, id, userID)
}

// ----------------------------------------------------------------- publishing

// Publish makes a draft public now, or schedules it when publishAt is more
// than a minute away. Every file in the release must have finished processing.
// Publishing an already public release changes nothing.
func (s *Store) Publish(ctx context.Context, id, userID string, publishAt *time.Time) (*Release, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var rtype, visibility string
	err = tx.QueryRow(ctx, `
		SELECT al.release_type, al.visibility FROM albums al JOIN artists a ON a.id = al.artist_id
		WHERE al.id = $1 AND a.owner_user_id = $2 FOR UPDATE OF al`, id, userID).Scan(&rtype, &visibility)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if visibility == "public" {
		return s.Get(ctx, id, userID)
	}

	var tracks, inflight int
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM tracks WHERE album_id = $1),
		(SELECT count(*) FROM uploads WHERE release_id = $1 AND status IN ('pending','queued','processing'))`, id).
		Scan(&tracks, &inflight); err != nil {
		return nil, err
	}
	if inflight > 0 {
		return nil, ErrUploadsPending
	}
	if tracks == 0 {
		return nil, &ValidationError{"the release has no finished tracks yet"}
	}
	if rtype == "single" && tracks != 1 {
		return nil, &ValidationError{"a single must have exactly one track (make it an album instead)"}
	}

	if publishAt != nil && publishAt.After(time.Now().Add(time.Minute)) {
		if publishAt.After(time.Now().AddDate(1, 0, 0)) {
			return nil, &ValidationError{"publishAt must be within the next year"}
		}
		if _, err := tx.Exec(ctx, `UPDATE albums SET visibility = 'scheduled', publish_at = $2, updated_at = now() WHERE id = $1`,
			id, publishAt.UTC()); err != nil {
			return nil, err
		}
	} else {
		if err := goPublic(ctx, tx, []string{id}); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, id, userID)
}

func goPublic(ctx context.Context, tx pgx.Tx, ids []string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE albums SET visibility = 'public', publish_at = NULL, published_at = COALESCE(published_at, now()),
		       release_date = COALESCE(release_date, CURRENT_DATE), updated_at = now()
		WHERE id = ANY($1)`, ids); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE tracks SET visibility = 'public' WHERE album_id = ANY($1)`, ids)
	return err
}

// Unpublish takes a public or scheduled release back to draft; its tracks
// stop appearing in search, playback and new library or playlist additions.
func (s *Store) Unpublish(ctx context.Context, id, userID string) (*Release, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var one int
	err = tx.QueryRow(ctx, `
		SELECT 1 FROM albums al JOIN artists a ON a.id = al.artist_id
		WHERE al.id = $1 AND a.owner_user_id = $2 FOR UPDATE OF al`, id, userID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE albums SET visibility = 'draft', publish_at = NULL, updated_at = now() WHERE id = $1`, id); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE tracks SET visibility = 'draft' WHERE album_id = $1`, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, id, userID)
}

// PublishDue makes scheduled releases whose time has come public. The worker
// calls it every few seconds. It returns how many went live.
func (s *Store) PublishDue(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, `
		SELECT id FROM albums WHERE visibility = 'scheduled' AND publish_at <= now()
		ORDER BY publish_at FOR UPDATE SKIP LOCKED LIMIT 100`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if err := goPublic(ctx, tx, ids); err != nil {
		return 0, err
	}
	return len(ids), tx.Commit(ctx)
}

// -------------------------------------------------------------------- tracks

// TrackPatch holds the editable track fields; nil means unchanged.
type TrackPatch struct {
	Title       *string
	Explicit    *bool
	Genre       *string
	TrackNumber *int // 0 clears the number
}

// UpdateTrack edits a track the user's artist owns.
func (s *Store) UpdateTrack(ctx context.Context, id, userID string, p TrackPatch) (*TrackView, error) {
	var t TrackView
	var number *int
	err := s.pool.QueryRow(ctx, `
		SELECT t.title, t.explicit, t.genre, t.track_number FROM tracks t JOIN artists a ON a.id = t.artist_id
		WHERE t.id = $1 AND a.owner_user_id = $2`, id, userID).Scan(&t.Title, &t.Explicit, &t.Genre, &number)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if p.Title != nil {
		t.Title = strings.TrimSpace(*p.Title)
	}
	if utf8.RuneCountInString(t.Title) < 1 || utf8.RuneCountInString(t.Title) > 200 {
		return nil, &ValidationError{"title must be 1-200 characters"}
	}
	if p.Explicit != nil {
		t.Explicit = *p.Explicit
	}
	if p.Genre != nil {
		t.Genre = strings.TrimSpace(*p.Genre)
		if utf8.RuneCountInString(t.Genre) > 40 {
			return nil, &ValidationError{"genre must be at most 40 characters"}
		}
	}
	if p.TrackNumber != nil {
		if *p.TrackNumber < 0 || *p.TrackNumber > 999 {
			return nil, &ValidationError{"trackNumber must be between 1 and 999"}
		}
		if *p.TrackNumber == 0 {
			number = nil
		} else {
			number = p.TrackNumber
		}
	}
	err = s.pool.QueryRow(ctx, `
		UPDATE tracks SET title=$2, explicit=$3, genre=$4, track_number=$5 WHERE id=$1
		RETURNING id, title, track_number, duration_ms, explicit, genre, visibility`,
		id, t.Title, t.Explicit, t.Genre, number).
		Scan(&t.ID, &t.Title, &t.TrackNumber, &t.DurationMs, &t.Explicit, &t.Genre, &t.Visibility)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// DeleteTrack removes a track, its files and its place in anyone's library or
// playlists, and frees the storage it used.
func (s *Store) DeleteTrack(ctx context.Context, id, userID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var albumID *string
	err = tx.QueryRow(ctx, `
		SELECT t.album_id FROM tracks t JOIN artists a ON a.id = t.artist_id
		WHERE t.id = $1 AND a.owner_user_id = $2 FOR UPDATE OF t`, id, userID).Scan(&albumID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	keys, err := deleteTracks(ctx, tx, []string{id})
	if err != nil {
		return err
	}
	if albumID != nil { // a release with nothing left in it cannot stay live
		if _, err := tx.Exec(ctx, `
			UPDATE albums SET visibility = 'draft', publish_at = NULL, updated_at = now()
			WHERE id = $1 AND visibility <> 'draft' AND NOT EXISTS (SELECT 1 FROM tracks WHERE album_id = $1)`, *albumID); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.removeObjects(ctx, keys)
	return nil
}

// Delete removes a release with all its tracks and unfinished uploads.
func (s *Store) Delete(ctx context.Context, id, userID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var one int
	err = tx.QueryRow(ctx, `
		SELECT 1 FROM albums al JOIN artists a ON a.id = al.artist_id
		WHERE al.id = $1 AND a.owner_user_id = $2 FOR UPDATE OF al`, id, userID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var busy bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM uploads WHERE release_id = $1 AND status = 'processing')`, id).Scan(&busy); err != nil {
		return err
	}
	if busy {
		return ErrProcessing
	}

	rows, err := tx.Query(ctx, `SELECT id FROM tracks WHERE album_id = $1`, id)
	if err != nil {
		return err
	}
	var trackIDs []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return err
		}
		trackIDs = append(trackIDs, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	keys, err := deleteTracks(ctx, tx, trackIDs)
	if err != nil {
		return err
	}
	// Uploads that never became tracks (pending, queued, failed).
	orows, err := tx.Query(ctx, `DELETE FROM uploads WHERE release_id = $1 RETURNING original_key`, id)
	if err != nil {
		return err
	}
	for orows.Next() {
		var k string
		if err := orows.Scan(&k); err != nil {
			orows.Close()
			return err
		}
		keys = append(keys, k)
	}
	orows.Close()
	if err := orows.Err(); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM albums WHERE id = $1`, id); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.removeObjects(ctx, keys)
	return nil
}

// deleteTracks deletes tracks and their upload rows inside tx, closes the gaps
// that leaves in playlists, and returns the storage keys to remove afterwards.
func deleteTracks(ctx context.Context, tx pgx.Tx, trackIDs []string) ([]string, error) {
	if len(trackIDs) == 0 {
		return nil, nil
	}
	var keys []string
	rows, err := tx.Query(ctx, `SELECT media_key FROM track_renditions WHERE track_id = ANY($1)`, trackIDs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	urows, err := tx.Query(ctx, `DELETE FROM uploads WHERE track_id = ANY($1) RETURNING original_key`, trackIDs)
	if err != nil {
		return nil, err
	}
	for urows.Next() {
		var k string
		if err := urows.Scan(&k); err != nil {
			urows.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	urows.Close()
	if err := urows.Err(); err != nil {
		return nil, err
	}

	// Playlists lose the items (ON DELETE CASCADE); renumber so positions stay 0..n-1.
	prows, err := tx.Query(ctx, `SELECT DISTINCT playlist_id FROM playlist_items WHERE track_id = ANY($1)`, trackIDs)
	if err != nil {
		return nil, err
	}
	var playlists []string
	for prows.Next() {
		var p string
		if err := prows.Scan(&p); err != nil {
			prows.Close()
			return nil, err
		}
		playlists = append(playlists, p)
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM tracks WHERE id = ANY($1)`, trackIDs); err != nil {
		return nil, err
	}
	if len(playlists) > 0 {
		if _, err := tx.Exec(ctx, `
			WITH ranked AS (
				SELECT id, row_number() OVER (PARTITION BY playlist_id ORDER BY position) - 1 AS rn
				FROM playlist_items WHERE playlist_id = ANY($1))
			UPDATE playlist_items p SET position = r.rn FROM ranked r WHERE p.id = r.id AND p.position <> r.rn`, playlists); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// removeObjects deletes storage objects after the database change committed.
// A failure leaves an orphan file but never an inconsistent catalog, so it is
// only logged.
func (s *Store) removeObjects(ctx context.Context, keys []string) {
	for _, k := range keys {
		if err := s.blobs.Remove(ctx, k); err != nil {
			logRemoveFailure(k, err)
		}
	}
}
