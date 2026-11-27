package release

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/genre"
	"github.com/blacmusic/backend/internal/image"
)

// ErrAlreadySingle: the song's release is a single already.
var ErrAlreadySingle = errors.New("release: this song is already a single")

// SplitInput describes the single made from a song of an album or EP.
type SplitInput struct {
	TrackID     string
	Title       string
	Genre       string // empty: the song's genre, else the album's
	ReleaseDate string // empty: the album's
	// The cover is a new upload (CoverImageID), the album's (UseAlbumCover), or neither.
	CoverImageID  string
	UseAlbumCover bool
	Public        bool // false leaves the single a draft
}

// SplitFrom is the album a song was taken out of.
type SplitFrom struct {
	ReleaseID, Title, ArtistID string
	Removed                    bool // it was the album's only song, so the album is gone
}

// AdminSplit moves one song of an album or EP into a new single, all or nothing. The song keeps its id, files,
// lyrics, credits and places in playlists, and its plays follow it. The other songs are renumbered; an album left
// with none is removed, and what pointed at it follows to the single. No audio or cover is deleted. adminID must
// own a newly uploaded cover; audit runs inside the same transaction. It returns the new release's id.
func (s *Store) AdminSplit(ctx context.Context, adminID string, in SplitInput,
	audit func(ctx context.Context, tx pgx.Tx, id string, from SplitFrom) error) (string, error) {
	if in.CoverImageID != "" && in.UseAlbumCover {
		return "", &ValidationError{"send coverImageId or useAlbumCover, not both"}
	}
	in.Title, in.Genre = strings.TrimSpace(in.Title), strings.TrimSpace(in.Genre)
	date, err := validate(Input{Type: "single", Title: in.Title, Genre: in.Genre, ReleaseDate: in.ReleaseDate})
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

	var from SplitFrom
	err = tx.QueryRow(ctx, `SELECT al.id, al.artist_id FROM tracks t JOIN albums al ON al.id = t.album_id
		WHERE t.id = $1 AND al.artist_id IS NOT NULL`, in.TrackID).Scan(&from.ReleaseID, &from.ArtistID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	// The artist first, as an upload does, so no new file can arrive in the album meanwhile.
	var banned bool
	if err := tx.QueryRow(ctx, `SELECT banned_at IS NOT NULL FROM artists WHERE id = $1 FOR UPDATE`, from.ArtistID).Scan(&banned); err != nil {
		return "", err
	}
	var kind, visibility string
	var takenDown, wasOnline bool
	var published, released *time.Time
	var albumGenre, trackGenre, cover, coverURL *string
	err = tx.QueryRow(ctx, `
		SELECT al.title, al.release_type, al.visibility, al.taken_down_at IS NOT NULL, al.published_at, al.release_date,
		       al.genre, t.genre, al.cover_image_id::text, al.artwork_url, t.visibility = 'public'
		FROM albums al JOIN tracks t ON t.album_id = al.id WHERE al.id = $1 AND t.id = $2 FOR UPDATE OF al, t`, from.ReleaseID, in.TrackID).
		Scan(&from.Title, &kind, &visibility, &takenDown, &published, &released, &albumGenre, &trackGenre, &cover, &coverURL, &wasOnline)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	wasOnline = wasOnline && visibility == "public"
	switch {
	case banned:
		return "", ErrArtistBanned
	case takenDown:
		return "", ErrTakenDown
	case kind == "single":
		return "", ErrAlreadySingle
	}
	var busy, last bool
	if err := tx.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM uploads WHERE release_id = $1 AND status IN ('pending', 'queued', 'processing'))
		OR EXISTS (SELECT 1 FROM transfer_items ti JOIN transfers tr ON tr.id = ti.transfer_id
		           WHERE ti.release_id = $1 AND ti.status NOT IN ('done', 'failed', 'dropped', 'skipped_duplicate')
		             AND tr.status IN ('collecting', 'queued', 'in_progress', 'retrying')),
		NOT EXISTS (SELECT 1 FROM tracks WHERE album_id = $1 AND id <> $2)`, from.ReleaseID, in.TrackID).Scan(&busy, &last); err != nil {
		return "", err
	}
	if busy {
		return "", ErrUploadsPending
	}
	from.Removed = last

	switch {
	case in.CoverImageID != "":
		if err := image.CheckOwned(ctx, tx, in.CoverImageID, adminID, "cover"); err != nil {
			return "", &ValidationError{"coverImageId must be one of your finished cover images"}
		}
		u := image.URL(s.publicURL, in.CoverImageID, image.SizeMedium)
		cover, coverURL = &in.CoverImageID, &u
	case !in.UseAlbumCover:
		cover, coverURL = nil, nil
	}
	if date == nil {
		date = released
	}
	for _, g := range []*string{trackGenre, albumGenre} {
		if in.Genre == "" && g != nil {
			in.Genre = *g
		}
	}
	if !in.Public || !wasOnline {
		published = nil
	}

	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO albums (title, artist_id, release_type, description, genre, release_date, cover_image_id, artwork_url, visibility, published_at)
		VALUES ($1, $2, 'single', '', $3, $4, $5, $6, 'draft', $7) RETURNING id`,
		in.Title, from.ArtistID, in.Genre, date, cover, coverURL, published).Scan(&id); err != nil {
		return "", err
	}

	// $1 the album, $2 the single, $3 the song
	moves := []string{
		// a credit of the whole album covered this song too
		`INSERT INTO release_credits (release_id, name, role, role_label, artist_id, position, by_team)
		 SELECT $2, name, role, role_label, artist_id, position, by_team FROM release_credits WHERE release_id = $1 AND track_id IS NULL AND $3 <> ''`,
		`UPDATE release_credits SET release_id = $2 WHERE release_id = $1 AND track_id = $3`,
		`UPDATE tracks SET album_id = $2, track_number = 1, visibility = 'draft', review_hold = false WHERE album_id = $1 AND id = $3`,
	}
	if last { // the album goes: everything that pointed at it now means the single
		moves = append(moves,
			`INSERT INTO library_albums (user_id, album_id, added_at) SELECT user_id, $2, added_at FROM library_albums WHERE album_id = $1 AND $3 <> ''`,
			`UPDATE plays SET release_id = $2 WHERE release_id = $1 AND $3 <> ''`,
			`UPDATE uploads SET release_id = $2 WHERE release_id = $1 AND $3 <> ''`,
			`UPDATE transfer_items SET release_id = $2 WHERE release_id = $1 AND $3 <> ''`,
			`UPDATE import_items SET release_id = $2 WHERE release_id = $1 AND $3 <> ''`,
			`UPDATE import_jobs SET album_release_id = $2 WHERE album_release_id = $1 AND $3 <> ''`,
			`UPDATE showcase_items SET ref_id = $2 WHERE kind = 'album' AND ref_id = $1 AND $3 <> ''`,
		)
	} else {
		moves = append(moves,
			`UPDATE plays SET release_id = $2 WHERE release_id = $1 AND track_id = $3`,
			`UPDATE uploads SET release_id = $2 WHERE release_id = $1 AND track_id = $3`,
			`UPDATE transfer_items SET release_id = $2 WHERE release_id = $1 AND track_id = $3`,
			`UPDATE import_items SET release_id = $2 WHERE release_id = $1 AND track_id = $3`,
		)
	}
	for _, q := range moves {
		if _, err := tx.Exec(ctx, q, from.ReleaseID, id, in.TrackID); err != nil {
			return "", err
		}
	}
	if last {
		// only the old row: nothing it pointed to is removed from storage
		if _, err := tx.Exec(ctx, `DELETE FROM albums WHERE id = $1`, from.ReleaseID); err != nil {
			return "", err
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE tracks t SET track_number = n.number
			FROM (SELECT id, row_number() OVER (ORDER BY track_number NULLS LAST, created_at, id) AS number FROM tracks WHERE album_id = $1) n
			WHERE t.id = n.id`, from.ReleaseID); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE albums SET updated_at = now() WHERE id = $1`, from.ReleaseID); err != nil {
			return "", err
		}
	}
	if in.Public {
		if err := goPublic(ctx, tx, []string{id}); err != nil {
			return "", err
		}
		// followers hear about music that is new to them, not about a song that was already online
		if !wasOnline && s.OnPublish != nil {
			if err := s.notifyLive(ctx, tx, []string{id}, s.OnPublish); err != nil {
				return "", err
			}
		}
	}
	if audit != nil {
		if err := audit(ctx, tx, id, from); err != nil {
			return "", err
		}
	}
	return id, tx.Commit(ctx)
}
