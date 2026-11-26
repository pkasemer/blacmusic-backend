// Package release implements releases (singles and albums), their draft /
// scheduled / public lifecycle, and editing or deleting the tracks in them.
package release

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/genre"
	"github.com/blacmusic/backend/internal/image"
)

var (
	ErrNotFound       = errors.New("release: not found")
	ErrNoArtist       = errors.New("release: this account has no artist profile")
	ErrUploadsPending = errors.New("release: some uploads are still being processed")
	ErrProcessing     = errors.New("release: a file is being processed right now")
	// ErrTakenDown: an administrator took the release down; only an administrator can bring it back.
	ErrTakenDown = errors.New("release: this release was taken down")
	// ErrArtistBanned: the artist profile is suspended.
	ErrArtistBanned = errors.New("release: this artist profile is suspended")
	// ErrNoTracks: a release needs at least one finished track before it can go live.
	ErrNoTracks = &ValidationError{"the release has no finished tracks yet"}
)

// ValidationError is returned for bad user input.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Blobs removes objects from storage.
type Blobs interface {
	Remove(ctx context.Context, key string) error
	AbortMultipart(ctx context.Context, key, uploadID string) error
}

// Store owns releases and the tracks inside them.
type Store struct {
	pool      *pgxpool.Pool
	blobs     Blobs
	publicURL string
	// OnLive, when set, is called for every scheduled release that goes public (the worker sets it).
	OnLive LiveHook
	// OnPublish, when set, is called when an artist publishes a release right away (the API sets it).
	OnPublish LiveHook
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
	ImageURL string `json:"imageUrl,omitempty"` // the artist's profile picture
	// ImageLargeURL is the same picture at about 1280 px.
	ImageLargeURL string `json:"imageLargeUrl,omitempty"`
}

// TrackView is a track inside a release.
type TrackView struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	TrackNumber  *int     `json:"trackNumber,omitempty"`
	DurationMs   int      `json:"durationMs"`
	Explicit     bool     `json:"explicit"`
	Genre        string   `json:"genre,omitempty"`
	Visibility   string   `json:"visibility,omitempty"` // owner only
	LoudnessLufs *float64 `json:"loudnessLufs,omitempty"`
	// ArtistVerified is the release artist's verification (the app shows a check mark). Always sent.
	ArtistVerified bool `json:"artistVerified"`
	HasLyrics      bool `json:"hasLyrics"`  // GET /v1/tracks/{id}/lyrics has something
	HasCredits     bool `json:"hasCredits"` // the track or its release has credits
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
	ID   string `json:"id"`
	Type string `json:"type"` // single | album
	// Kind is what the app tags the release with: single, ep (an album of 1-6 tracks) or album (7 or more).
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Genre       string `json:"genre,omitempty"`
	ReleaseDate string `json:"releaseDate,omitempty"` // YYYY-MM-DD
	ArtworkURL  string `json:"artworkUrl,omitempty"`
	// ArtworkLargeURL is the cover at about 1280 px (for a big hero); absent when there is none.
	ArtworkLargeURL string `json:"artworkLargeUrl,omitempty"`
	// TakenDown: an administrator took this release down (it is hidden like a draft and cannot be published).
	// TakedownReason is shown to the owner only.
	TakenDown      bool   `json:"takenDown"`
	TakedownReason string `json:"takedownReason,omitempty"`
	// HasCredits is true when the release or one of its tracks has credits (GET /v1/releases/{id}/credits).
	HasCredits bool `json:"hasCredits"`
	// TotalDurationSec is the length of all the tracks together, in seconds.
	TotalDurationSec int        `json:"totalDurationSec"`
	Visibility       string     `json:"visibility"` // draft | scheduled | public
	PublishAt        *time.Time `json:"publishAt,omitempty"`
	PublishedAt      *time.Time `json:"publishedAt,omitempty"`
	Artist           *ArtistRef `json:"artist,omitempty"`
	TrackCount       int        `json:"trackCount"`
	Mine             bool       `json:"mine,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`

	Tracks  []TrackView  `json:"tracks,omitempty"`
	Uploads []UploadView `json:"uploads,omitempty"` // owner only: unfinished and failed files
}

const releaseSelect = `
	SELECT al.id, al.release_type, al.title, al.description, al.genre, COALESCE(to_char(al.release_date, 'YYYY-MM-DD'), ''),
	       COALESCE(al.artwork_url, ''), al.visibility, al.publish_at, al.published_at, al.created_at,
	       COALESCE(a.id, ''), COALESCE(a.name, ''), COALESCE(a.verification = 'verified', false), COALESCE(a.owner_user_id::text, ''),
	       (SELECT count(*) FROM tracks t WHERE t.album_id = al.id),
	       COALESCE(au.avatar_image_id, a.avatar_image_id), COALESCE(a.image_url, ''),
	       (SELECT COALESCE(sum(t.duration_ms), 0) / 1000 FROM tracks t WHERE t.album_id = al.id),
	       EXISTS (SELECT 1 FROM release_credits rc WHERE rc.release_id = al.id),
	       al.taken_down_at IS NOT NULL, al.takedown_reason
	FROM albums al LEFT JOIN artists a ON a.id = al.artist_id LEFT JOIN users au ON au.id = a.owner_user_id`

func (s *Store) scanRelease(row pgx.Row, viewer string) (*Release, error) {
	var r Release
	var ar ArtistRef
	var owner, legacyImage string
	var avatarID *string
	err := row.Scan(&r.ID, &r.Type, &r.Title, &r.Description, &r.Genre, &r.ReleaseDate, &r.ArtworkURL, &r.Visibility,
		&r.PublishAt, &r.PublishedAt, &r.CreatedAt, &ar.ID, &ar.Name, &ar.Verified, &owner, &r.TrackCount,
		&avatarID, &legacyImage, &r.TotalDurationSec, &r.HasCredits, &r.TakenDown, &r.TakedownReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if ar.ID != "" {
		ar.ImageURL = image.PersonURL(s.publicURL, avatarID, legacyImage)
		ar.ImageLargeURL = image.PersonLargeURL(s.publicURL, avatarID)
		r.Artist = &ar
	}
	r.ArtworkLargeURL = image.LargeURL(r.ArtworkURL)
	r.Mine = viewer != "" && owner == viewer
	if !r.Mine {
		r.PublishAt = nil
		r.TakedownReason = ""
	}
	r.Kind = KindOf(r.Type, r.TrackCount)
	return &r, nil
}

// EPMaxTracks is the most tracks an album can have and still be called an EP.
const EPMaxTracks = 6

// KindOf says how a release is shown: "single", "ep" or "album".
func KindOf(releaseType string, tracks int) string {
	switch {
	case releaseType == "single":
		return "single"
	case tracks <= EPMaxTracks:
		return "ep"
	}
	return "album"
}

// Get returns a release. Drafts and scheduled releases are visible to their
// owner only; everyone else gets ErrNotFound.
func (s *Store) Get(ctx context.Context, id, viewer string) (*Release, error) {
	r, err := s.scanRelease(s.pool.QueryRow(ctx, releaseSelect+` WHERE al.id = $1`, id), viewer)
	if err != nil {
		return nil, err
	}
	if r.Visibility != "public" && !r.Mine {
		return nil, ErrNotFound
	}

	q := `SELECT t.id, t.title, t.track_number, t.duration_ms, t.explicit, t.genre, t.visibility, t.loudness_lufs,
	             EXISTS (SELECT 1 FROM track_lyrics tl WHERE tl.track_id = t.id),
	             EXISTS (SELECT 1 FROM release_credits rc WHERE rc.track_id = t.id OR (rc.track_id IS NULL AND rc.release_id = t.album_id)) FROM tracks t
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
		if err := rows.Scan(&t.ID, &t.Title, &t.TrackNumber, &t.DurationMs, &t.Explicit, &t.Genre, &t.Visibility, &t.LoudnessLufs, &t.HasLyrics, &t.HasCredits); err != nil {
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
	r.Kind = KindOf(r.Type, r.TrackCount)
	r.TotalDurationSec = 0
	for _, t := range r.Tracks {
		r.TotalDurationSec += t.DurationMs / 1000
	}
	for i := range r.Tracks {
		r.Tracks[i].ArtistVerified = r.Artist != nil && r.Artist.Verified
	}

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
		r, err := s.scanRelease(rows, viewer)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Search finds public releases whose title or artist name contains q (case-insensitive), of the given
// kind: all (default), single, ep or album. Title matches come first (exact, then starting with q, then a
// word starting with q, then containing q), then matches on the artist name only; ties go to verified
// artists, then the newest release.
func (s *Store) Search(ctx context.Context, q, kind string, limit, offset int) ([]*Release, error) {
	cond := ""
	switch kind {
	case "", "all":
	case "single":
		cond = " AND al.release_type = 'single'"
	case "ep":
		cond = " AND al.release_type = 'album' AND (SELECT count(*) FROM tracks t WHERE t.album_id = al.id) <= " + strconv.Itoa(EPMaxTracks)
	case "album":
		cond = " AND al.release_type = 'album' AND (SELECT count(*) FROM tracks t WHERE t.album_id = al.id) > " + strconv.Itoa(EPMaxTracks)
	default:
		return nil, &ValidationError{"type must be all, single, ep or album"}
	}
	esc := likeEscape(q)
	rows, err := s.pool.Query(ctx, releaseSelect+`
		WHERE al.visibility = 'public'
		  AND EXISTS (SELECT 1 FROM tracks t WHERE t.album_id = al.id AND t.visibility = 'public')
		  AND (al.title ILIKE $4 OR a.name ILIKE $4)`+cond+`
		ORDER BY CASE WHEN lower(al.title) = lower($1) THEN 0 WHEN al.title ILIKE $2 THEN 1 WHEN al.title ILIKE $3 THEN 2
		              WHEN al.title ILIKE $4 THEN 3 ELSE 4 END,
		         COALESCE(a.verification = 'verified', false) DESC, COALESCE(al.published_at, al.created_at) DESC, al.id
		LIMIT $5 OFFSET $6`, q, esc+"%", "% "+esc+"%", "%"+esc+"%", limit, offset)
	if err != nil {
		return nil, err
	}
	return s.collect(rows, "")
}

// Related suggests public releases to listen to after one: those sharing a genre with it (the release's own
// genre or its tracks' genres), most shared genres first, then the artist's other public releases. Never
// the release itself. ErrNotFound when the viewer may not see the release.
func (s *Store) Related(ctx context.Context, id, viewer string, limit int) ([]*Release, error) {
	base, err := s.Get(ctx, id, viewer)
	if err != nil {
		return nil, err
	}
	artistID := ""
	if base.Artist != nil {
		artistID = base.Artist.ID
	}
	var genres []string
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(array_agg(DISTINCT g), '{}') FROM (
			SELECT lower(al.genre) AS g FROM albums al WHERE al.id = $1
			UNION SELECT lower(t.genre) FROM tracks t WHERE t.album_id = $1) x WHERE g <> ''`, id).Scan(&genres); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, releaseSelect+`
		WHERE al.id <> $1 AND al.visibility = 'public'
		  AND EXISTS (SELECT 1 FROM tracks t WHERE t.album_id = al.id AND t.visibility = 'public')
		  AND (al.artist_id = $2 OR `+relatedOverlap+` > 0)
		ORDER BY `+relatedOverlap+` DESC, COALESCE(a.verification = 'verified', false) DESC,
		         COALESCE(al.published_at, al.created_at) DESC, al.id
		LIMIT $4`, id, artistID, genres, limit)
	if err != nil {
		return nil, err
	}
	return s.collect(rows, "")
}

// relatedOverlap counts the genres a release (al) shares with the list in $3.
const relatedOverlap = `(SELECT count(*) FROM (
		SELECT lower(al.genre) AS g UNION SELECT lower(t.genre) FROM tracks t WHERE t.album_id = al.id) og
	WHERE og.g <> '' AND og.g = ANY($3::text[]))`

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

// Browse lists live releases for the home screen.
//
//	typ: "album" (albums and EPs), "single" or "all"
//	sort: "new" (most recently released first) or "popular" (most saved by listeners)
//
// Only public releases that have something to play are returned.
func (s *Store) Browse(ctx context.Context, sort, typ string, limit, offset int) ([]*Release, error) {
	order := "COALESCE(al.published_at, al.created_at) DESC, al.id"
	switch sort {
	case "", "new":
	case "popular":
		order = `((SELECT count(*) FROM library_albums la WHERE la.album_id = al.id)
		        + (SELECT count(*) FROM library_tracks lt JOIN tracks tt ON tt.id = lt.track_id WHERE tt.album_id = al.id)) DESC, ` + order
	default:
		return nil, &ValidationError{"sort must be new or popular"}
	}
	switch typ {
	case "", "album", "single", "all":
	default:
		return nil, &ValidationError{"type must be album, single or all"}
	}
	if typ == "" {
		typ = "album"
	}
	rows, err := s.pool.Query(ctx, releaseSelect+`
		WHERE al.visibility = 'public'
		  AND EXISTS (SELECT 1 FROM tracks t WHERE t.album_id = al.id AND t.visibility = 'public')
		  AND ($1 = 'all' OR al.release_type = $1)
		ORDER BY `+order+` LIMIT $2 OFFSET $3`, typ, limit, offset)
	if err != nil {
		return nil, err
	}
	return s.collect(rows, "")
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
	artistID, err := artist.IDByOwner(ctx, s.pool, userID)
	if errors.Is(err, artist.ErrNotFound) {
		return nil, ErrNoArtist
	}
	if err != nil {
		return nil, err
	}
	id, err := s.create(ctx, artistID, userID, in, nil)
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id, userID)
}

// AdminCreate creates a draft release for any artist (an administrator action; the cover must be one of the
// administrator finished cover images). audit runs inside the same transaction. It returns the new id.
func (s *Store) AdminCreate(ctx context.Context, artistID, adminID string, in Input, audit func(ctx context.Context, tx pgx.Tx, id string) error) (string, error) {
	var banned bool
	if err := s.pool.QueryRow(ctx, `SELECT banned_at IS NOT NULL FROM artists WHERE id = $1`, artistID).Scan(&banned); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if banned {
		return "", ErrArtistBanned
	}
	return s.create(ctx, artistID, adminID, in, audit)
}

// create validates and inserts a draft release for artistID; coverOwner is who must own the cover image.
func (s *Store) create(ctx context.Context, artistID, coverOwner string, in Input, audit func(ctx context.Context, tx pgx.Tx, id string) error) (string, error) {
	in.Title, in.Description, in.Genre = strings.TrimSpace(in.Title), strings.TrimSpace(in.Description), strings.TrimSpace(in.Genre)
	date, err := validate(in)
	if err != nil {
		return "", err
	}
	if in.Genre, err = genre.Normalize(in.Genre); err != nil {
		return "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var cover, coverURL *string
	if in.CoverImageID != "" {
		if err := image.CheckOwned(ctx, tx, in.CoverImageID, coverOwner, "cover"); err != nil {
			return "", &ValidationError{"coverImageId must be one of your finished cover images"}
		}
		u := image.URL(s.publicURL, in.CoverImageID, image.SizeMedium)
		cover, coverURL = &in.CoverImageID, &u
	}
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO albums (title, artist_id, release_type, description, genre, release_date, cover_image_id, artwork_url, visibility)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'draft') RETURNING id`,
		in.Title, artistID, in.Type, in.Description, in.Genre, date, cover, coverURL).Scan(&id)
	if err != nil {
		return "", err
	}
	if audit != nil {
		if err := audit(ctx, tx, id); err != nil {
			return "", err
		}
	}
	return id, tx.Commit(ctx)
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
		g := strings.TrimSpace(*p.Genre)
		if g != in.Genre { // a genre saved before the list existed stays as it is until it is changed
			if utf8.RuneCountInString(g) > 40 {
				return nil, &ValidationError{"genre must be at most 40 characters"}
			}
			if g, err = genre.Normalize(g); err != nil {
				return nil, err
			}
		}
		in.Genre = g
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
	if err := s.publish(ctx, id, userID, publishAt, nil); err != nil {
		return nil, err
	}
	return s.Get(ctx, id, userID)
}

// AdminPublish publishes or schedules any release with the owner rules (an administrator action). audit runs
// inside the same transaction.
func (s *Store) AdminPublish(ctx context.Context, id string, publishAt *time.Time, audit func(ctx context.Context, tx pgx.Tx) error) error {
	return s.publish(ctx, id, "", publishAt, audit)
}

// publish does the work of Publish. userID "" skips the ownership check (administrators).
func (s *Store) publish(ctx context.Context, id, userID string, publishAt *time.Time, audit func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var rtype, visibility string
	var takenDown, banned bool
	if userID == "" {
		err = tx.QueryRow(ctx, `
			SELECT al.release_type, al.visibility, al.taken_down_at IS NOT NULL, COALESCE(a.banned_at IS NOT NULL, false)
			FROM albums al LEFT JOIN artists a ON a.id = al.artist_id
			WHERE al.id = $1 FOR UPDATE OF al`, id).Scan(&rtype, &visibility, &takenDown, &banned)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT al.release_type, al.visibility, al.taken_down_at IS NOT NULL, a.banned_at IS NOT NULL
			FROM albums al JOIN artists a ON a.id = al.artist_id
			WHERE al.id = $1 AND a.owner_user_id = $2 FOR UPDATE OF al`, id, userID).Scan(&rtype, &visibility, &takenDown, &banned)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if takenDown {
		return ErrTakenDown
	}
	if banned {
		return ErrArtistBanned
	}
	if visibility == "public" {
		return nil
	}

	var tracks, inflight int
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM tracks WHERE album_id = $1),
		(SELECT count(*) FROM uploads WHERE release_id = $1 AND status IN ('pending','queued','processing'))`, id).
		Scan(&tracks, &inflight); err != nil {
		return err
	}
	if inflight > 0 {
		return ErrUploadsPending
	}
	if tracks == 0 {
		return ErrNoTracks
	}
	if rtype == "single" && tracks != 1 {
		return &ValidationError{"a single must have exactly one track (make it an album instead)"}
	}

	if publishAt != nil && publishAt.After(time.Now().Add(time.Minute)) {
		if publishAt.After(time.Now().AddDate(1, 0, 0)) {
			return &ValidationError{"publishAt must be within the next year"}
		}
		if _, err := tx.Exec(ctx, `UPDATE albums SET visibility = 'scheduled', publish_at = $2, updated_at = now() WHERE id = $1`,
			id, publishAt.UTC()); err != nil {
			return err
		}
	} else {
		if err := goPublic(ctx, tx, []string{id}); err != nil {
			return err
		}
		if s.OnPublish != nil {
			if err := s.notifyLive(ctx, tx, []string{id}, s.OnPublish); err != nil {
				return err
			}
		}
	}
	if audit != nil {
		if err := audit(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// notifyLive tells OnLive about releases that just went public. A failing hook runs in a
// savepoint, so it can never stop a release from going live.
func (s *Store) notifyLive(ctx context.Context, tx pgx.Tx, ids []string, hook LiveHook) error {
	rows, err := tx.Query(ctx, `SELECT al.id, al.title, a.owner_user_id::text FROM albums al JOIN artists a ON a.id = al.artist_id
		WHERE al.id = ANY($1) AND a.owner_user_id IS NOT NULL`, ids)
	if err != nil {
		return err
	}
	type live struct{ id, title, owner string }
	var evs []live
	for rows.Next() {
		var e live
		if err := rows.Scan(&e.id, &e.title, &e.owner); err != nil {
			rows.Close()
			return err
		}
		evs = append(evs, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range evs {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		if herr := hook(ctx, sp, e.id, e.title, e.owner); herr != nil {
			slog.Error("release live hook", "release", e.id, "err", herr)
			_ = sp.Rollback(ctx)
			continue
		}
		if err := sp.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func goPublic(ctx context.Context, tx pgx.Tx, ids []string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE albums SET visibility = 'public', publish_at = NULL, published_at = COALESCE(published_at, now()),
		       release_date = COALESCE(release_date, (now() AT TIME ZONE 'Africa/Kampala')::date), updated_at = now()
		WHERE id = ANY($1)`, ids); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE tracks SET visibility = 'public' WHERE album_id = ANY($1)`, ids)
	return err
}

// Unpublish takes a public or scheduled release back to draft; its tracks
// stop appearing in search, playback and new library or playlist additions.
func (s *Store) Unpublish(ctx context.Context, id, userID string) (*Release, error) {
	if err := s.unpublish(ctx, id, userID, nil); err != nil {
		return nil, err
	}
	return s.Get(ctx, id, userID)
}

// AdminUnpublish takes any release back to draft (an administrator action); audit runs inside the same transaction.
func (s *Store) AdminUnpublish(ctx context.Context, id string, audit func(ctx context.Context, tx pgx.Tx) error) error {
	return s.unpublish(ctx, id, "", audit)
}

func (s *Store) unpublish(ctx context.Context, id, userID string, audit func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var one int
	if userID == "" {
		err = tx.QueryRow(ctx, `SELECT 1 FROM albums WHERE id = $1 FOR UPDATE`, id).Scan(&one)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT 1 FROM albums al JOIN artists a ON a.id = al.artist_id
			WHERE al.id = $1 AND a.owner_user_id = $2 FOR UPDATE OF al`, id, userID).Scan(&one)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE albums SET visibility = 'draft', publish_at = NULL, updated_at = now() WHERE id = $1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE tracks SET visibility = 'draft' WHERE album_id = $1`, id); err != nil {
		return err
	}
	if audit != nil {
		if err := audit(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// LiveHook is told, inside the transaction that makes a release public, about each release that
// goes live (so a notification is queued exactly when the release is).
type LiveHook func(ctx context.Context, tx pgx.Tx, releaseID, title, ownerUserID string) error

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
	if s.OnLive != nil {
		if err := s.notifyLive(ctx, tx, ids, s.OnLive); err != nil {
			return 0, err
		}
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
		SELECT t.title, t.explicit, t.genre, t.track_number, a.verification = 'verified',
		       EXISTS (SELECT 1 FROM track_lyrics tl WHERE tl.track_id = t.id),
		       EXISTS (SELECT 1 FROM release_credits rc WHERE rc.track_id = t.id OR (rc.track_id IS NULL AND rc.release_id = t.album_id))
		FROM tracks t JOIN artists a ON a.id = t.artist_id
		WHERE t.id = $1 AND a.owner_user_id = $2`, id, userID).Scan(&t.Title, &t.Explicit, &t.Genre, &number, &t.ArtistVerified, &t.HasLyrics, &t.HasCredits)
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
		g := strings.TrimSpace(*p.Genre)
		if g != t.Genre { // an existing free-text genre is left alone unless it is being changed
			if utf8.RuneCountInString(g) > 40 {
				return nil, &ValidationError{"genre must be at most 40 characters"}
			}
			var gerr error
			if g, gerr = genre.Normalize(g); gerr != nil {
				return nil, gerr
			}
		}
		t.Genre = g
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
		RETURNING id, title, track_number, duration_ms, explicit, genre, visibility, loudness_lufs`,
		id, t.Title, t.Explicit, t.Genre, number).
		Scan(&t.ID, &t.Title, &t.TrackNumber, &t.DurationMs, &t.Explicit, &t.Genre, &t.Visibility, &t.LoudnessLufs)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// DeleteTrack removes a track, its files and its place in anyone's library or
// playlists, and frees the storage it used.
func (s *Store) DeleteTrack(ctx context.Context, id, userID string) error {
	return s.deleteTrack(ctx, id, userID, nil)
}

// AdminDeleteTrack removes any track (an administrator's action); audit runs inside the same transaction.
func (s *Store) AdminDeleteTrack(ctx context.Context, id string, audit func(ctx context.Context, tx pgx.Tx) error) error {
	return s.deleteTrack(ctx, id, "", audit)
}

// deleteTrack deletes a track. userID "" skips the ownership check (administrators).
func (s *Store) deleteTrack(ctx context.Context, id, userID string, audit func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var albumID *string
	if userID == "" {
		err = tx.QueryRow(ctx, `SELECT t.album_id FROM tracks t WHERE t.id = $1 FOR UPDATE OF t`, id).Scan(&albumID)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT t.album_id FROM tracks t JOIN artists a ON a.id = t.artist_id
			WHERE t.id = $1 AND a.owner_user_id = $2 FOR UPDATE OF t`, id, userID).Scan(&albumID)
	}
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
	if audit != nil {
		if err := audit(ctx, tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.removeObjects(ctx, keys)
	return nil
}

// PurgeArtistUploads deletes an artist's remaining upload rows (files that never became part of a release)
// and their stored files, aborting open multipart uploads. ErrProcessing while one is being processed.
func (s *Store) PurgeArtistUploads(ctx context.Context, artistID string) error {
	rows, err := s.pool.Query(ctx, `DELETE FROM uploads WHERE artist_id = $1 AND status <> 'processing' RETURNING original_key, multipart_id`, artistID)
	if err != nil {
		return err
	}
	type obj struct{ key, multipart string }
	var objs []obj
	for rows.Next() {
		var o obj
		if err := rows.Scan(&o.key, &o.multipart); err != nil {
			rows.Close()
			return err
		}
		objs = append(objs, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range objs {
		if o.multipart != "" {
			if err := s.blobs.AbortMultipart(ctx, o.key, o.multipart); err != nil {
				logRemoveFailure(o.key, err)
			}
		}
	}
	s.removeObjects(ctx, func() []string {
		keys := make([]string, len(objs))
		for i, o := range objs {
			keys[i] = o.key
		}
		return keys
	}())
	var busy bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM uploads WHERE artist_id = $1)`, artistID).Scan(&busy); err != nil {
		return err
	}
	if busy {
		return ErrProcessing
	}
	return nil
}

// Delete removes a release with all its tracks and unfinished uploads.
func (s *Store) Delete(ctx context.Context, id, userID string) error {
	return s.deleteRelease(ctx, id, userID, nil)
}

// AdminDelete removes any release (an administrator's action). audit, when set, runs inside the same
// transaction as the deletion so the log row and the deletion commit together.
func (s *Store) AdminDelete(ctx context.Context, id string, audit func(ctx context.Context, tx pgx.Tx) error) error {
	return s.deleteRelease(ctx, id, "", audit)
}

// deleteRelease deletes a release. userID "" skips the ownership check (administrators).
func (s *Store) deleteRelease(ctx context.Context, id, userID string, audit func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var one int
	if userID == "" {
		err = tx.QueryRow(ctx, `SELECT 1 FROM albums WHERE id = $1 FOR UPDATE`, id).Scan(&one)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT 1 FROM albums al JOIN artists a ON a.id = al.artist_id
			WHERE al.id = $1 AND a.owner_user_id = $2 FOR UPDATE OF al`, id, userID).Scan(&one)
	}
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
	// Uploads that never became tracks (pending, queued, failed). An unfinished
	// multipart upload must be aborted or its parts keep using storage.
	orows, err := tx.Query(ctx, `DELETE FROM uploads WHERE release_id = $1 RETURNING original_key, multipart_id`, id)
	if err != nil {
		return err
	}
	var multiparts [][2]string
	for orows.Next() {
		var k, mp string
		if err := orows.Scan(&k, &mp); err != nil {
			orows.Close()
			return err
		}
		keys = append(keys, k)
		if mp != "" {
			multiparts = append(multiparts, [2]string{k, mp})
		}
	}
	orows.Close()
	if err := orows.Err(); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM albums WHERE id = $1`, id); err != nil {
		return err
	}
	if audit != nil {
		if err := audit(ctx, tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	for _, m := range multiparts {
		if err := s.blobs.AbortMultipart(ctx, m[0], m[1]); err != nil {
			logRemoveFailure(m[0], err)
		}
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
