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

// MaxCombine is the most releases that can be combined at once.
const MaxCombine = 50

// ErrDifferentArtists: only one artist's releases can be combined.
var ErrDifferentArtists = errors.New("release: the releases belong to different artists")

// CombineInput says which releases become one album or EP.
type CombineInput struct {
	ReleaseIDs  []string
	TrackIDs    []string // every track of those releases once, in the new album's order
	Title       string
	Genre       string // empty: the releases' genre when they all share one
	ReleaseDate string // empty: the newest of the releases' dates
	// The cover is a new upload (CoverImageID), the one of a combined release (CoverFromReleaseID), or neither.
	CoverImageID       string
	CoverFromReleaseID string
	Public             bool // false leaves the album a draft
}

// Combined is one of the releases that went into a new album.
type Combined struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// AdminCombine moves every song of several releases of one artist into a new album and removes the old releases,
// all or nothing. Songs keep their ids, files, lyrics and places in playlists; plays, credits, listeners' saved
// releases and the signed-out showcase follow to the album. No audio or cover is deleted. adminID must own a newly
// uploaded cover; audit runs inside the same transaction. It returns the new release's id.
func (s *Store) AdminCombine(ctx context.Context, adminID string, in CombineInput,
	audit func(ctx context.Context, tx pgx.Tx, id, artistID string, from []Combined) error) (string, error) {
	ids := map[string]bool{}
	for _, id := range in.ReleaseIDs {
		ids[id] = true
	}
	if len(ids) != len(in.ReleaseIDs) || len(ids) < 2 || len(ids) > MaxCombine {
		return "", &ValidationError{"releaseIds must be 2-50 different releases"}
	}
	if in.CoverImageID != "" && in.CoverFromReleaseID != "" {
		return "", &ValidationError{"send coverImageId or coverFromReleaseId, not both"}
	}
	if in.CoverFromReleaseID != "" && !ids[in.CoverFromReleaseID] {
		return "", &ValidationError{"coverFromReleaseId must be one of releaseIds"}
	}
	in.Title, in.Genre = strings.TrimSpace(in.Title), strings.TrimSpace(in.Genre)
	date, err := validate(Input{Type: "album", Title: in.Title, Genre: in.Genre, ReleaseDate: in.ReleaseDate})
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

	var found, artists int
	var artistID string
	if err := tx.QueryRow(ctx, `SELECT count(*), count(DISTINCT COALESCE(artist_id, id)), COALESCE(min(artist_id), '')
		FROM albums WHERE id = ANY($1)`, in.ReleaseIDs).Scan(&found, &artists, &artistID); err != nil {
		return "", err
	}
	if found != len(ids) {
		return "", ErrNotFound
	}
	if artists != 1 || artistID == "" {
		return "", ErrDifferentArtists
	}
	// The artist first, as an upload does, so no new file can arrive in these releases while they are combined.
	var banned bool
	if err := tx.QueryRow(ctx, `SELECT banned_at IS NOT NULL FROM artists WHERE id = $1 FOR UPDATE`, artistID).Scan(&banned); err != nil {
		return "", err
	}
	if banned {
		return "", ErrArtistBanned
	}

	rows, err := tx.Query(ctx, `
		SELECT id, title, visibility, taken_down_at IS NOT NULL, published_at, release_date, genre,
		       cover_image_id::text, artwork_url, (SELECT count(*) FROM tracks t WHERE t.album_id = al.id)
		FROM albums al WHERE id = ANY($1) AND artist_id = $2 ORDER BY created_at, id FOR UPDATE OF al`, in.ReleaseIDs, artistID)
	if err != nil {
		return "", err
	}
	var from []Combined
	var cover, coverURL, newestDate = (*string)(nil), (*string)(nil), (*time.Time)(nil)
	var publishedAt *time.Time // the earliest of those already online, so the album does not pose as brand new
	wasPublic, takenDown, empty := false, false, false
	genres := map[string]bool{}
	for rows.Next() {
		var c Combined
		var visibility string
		var down bool
		var published, released *time.Time
		var g, imageID, url *string
		var tracks int
		if err := rows.Scan(&c.ID, &c.Title, &visibility, &down, &published, &released, &g, &imageID, &url, &tracks); err != nil {
			rows.Close()
			return "", err
		}
		from = append(from, c)
		takenDown, empty = takenDown || down, empty || tracks == 0
		if visibility == "public" {
			wasPublic = true
			if published != nil && (publishedAt == nil || published.Before(*publishedAt)) {
				publishedAt = published
			}
		}
		if released != nil && (newestDate == nil || released.After(*newestDate)) {
			newestDate = released
		}
		if g != nil {
			genres[*g] = true
		} else {
			genres[""] = true
		}
		if c.ID == in.CoverFromReleaseID {
			cover, coverURL = imageID, url
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch {
	case len(from) != len(ids):
		return "", ErrNotFound
	case takenDown:
		return "", ErrTakenDown
	}

	var busy bool
	if err := tx.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM uploads WHERE release_id = ANY($1) AND status IN ('pending', 'queued', 'processing'))
		OR EXISTS (SELECT 1 FROM transfer_items ti JOIN transfers tr ON tr.id = ti.transfer_id
		           WHERE ti.release_id = ANY($1) AND ti.status NOT IN ('done', 'failed', 'dropped', 'skipped_duplicate')
		             AND tr.status IN ('collecting', 'queued', 'in_progress', 'retrying'))`, in.ReleaseIDs).Scan(&busy); err != nil {
		return "", err
	}
	if busy {
		return "", ErrUploadsPending
	}
	if empty {
		return "", ErrNoTracks
	}

	// the order must name each of the songs once
	var complete bool
	if err := tx.QueryRow(ctx, `
		WITH have AS (SELECT id FROM tracks WHERE album_id = ANY($1)), named AS (SELECT unnest($2::text[]) AS id)
		SELECT (SELECT count(*) FROM named) = (SELECT count(*) FROM have)
		   AND (SELECT count(DISTINCT id) FROM named) = (SELECT count(*) FROM have)
		   AND NOT EXISTS (SELECT id FROM have EXCEPT SELECT id FROM named)`, in.ReleaseIDs, in.TrackIDs).Scan(&complete); err != nil {
		return "", err
	}
	if !complete {
		return "", &ValidationError{"trackIds must list every track of those releases exactly once"}
	}

	if in.CoverImageID != "" {
		if err := image.CheckOwned(ctx, tx, in.CoverImageID, adminID, "cover"); err != nil {
			return "", &ValidationError{"coverImageId must be one of your finished cover images"}
		}
		u := image.URL(s.publicURL, in.CoverImageID, image.SizeMedium)
		cover, coverURL = &in.CoverImageID, &u
	}
	if date == nil {
		date = newestDate
	}
	if in.Genre == "" && len(genres) == 1 {
		for g := range genres {
			in.Genre = g
		}
	}
	if !in.Public {
		publishedAt = nil
	}

	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO albums (title, artist_id, release_type, description, genre, release_date, cover_image_id, artwork_url, visibility, published_at)
		VALUES ($1, $2, 'album', '', $3, $4, $5, $6, 'draft', $7) RETURNING id`,
		in.Title, artistID, in.Genre, date, cover, coverURL, publishedAt).Scan(&id); err != nil {
		return "", err
	}

	// Credits on a whole old release stay a credit of the whole album only when every release had it; otherwise they
	// become credits of that release's songs. This runs while the songs are still in their old releases.
	const sameCredit = `o.name = c.name AND o.role = c.role AND o.role_label = c.role_label AND o.artist_id IS NOT DISTINCT FROM c.artist_id`
	const onAll = `(SELECT count(DISTINCT o.release_id) FROM release_credits o WHERE o.release_id = ANY($1) AND o.track_id IS NULL AND ` + sameCredit + `) = cardinality($1::text[])`
	for _, q := range []string{
		`INSERT INTO release_credits (release_id, name, role, role_label, artist_id, position, by_team)
		 SELECT $2, name, role, role_label, artist_id, min(position), bool_or(by_team) FROM release_credits c
		 WHERE c.release_id = ANY($1) AND c.track_id IS NULL AND ` + onAll + ` GROUP BY name, role, role_label, artist_id`,
		`INSERT INTO release_credits (release_id, track_id, name, role, role_label, artist_id, position, by_team)
		 SELECT $2, t.id, c.name, c.role, c.role_label, c.artist_id, c.position, c.by_team FROM release_credits c JOIN tracks t ON t.album_id = c.release_id
		 WHERE c.release_id = ANY($1) AND c.track_id IS NULL AND NOT ` + onAll,
		`UPDATE release_credits SET release_id = $2 WHERE release_id = ANY($1) AND track_id IS NOT NULL`,
		`INSERT INTO library_albums (user_id, album_id, added_at)
		 SELECT user_id, $2, min(added_at) FROM library_albums WHERE album_id = ANY($1) GROUP BY user_id`,
		`UPDATE plays SET release_id = $2 WHERE release_id = ANY($1)`,
		`UPDATE uploads SET release_id = $2 WHERE release_id = ANY($1)`,
		`UPDATE transfer_items SET release_id = $2 WHERE release_id = ANY($1)`,
		`UPDATE import_items SET release_id = $2 WHERE release_id = ANY($1)`,
		`UPDATE import_jobs SET album_release_id = $2 WHERE album_release_id = ANY($1)`,
		// one showcase entry stands for the album, where the first of the old ones was
		`UPDATE showcase_items SET ref_id = $2 WHERE position = (SELECT min(position) FROM showcase_items WHERE kind = 'album' AND ref_id = ANY($1))`,
	} {
		if _, err := tx.Exec(ctx, q, in.ReleaseIDs, id); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tracks t SET album_id = $1, track_number = n.number, visibility = 'draft', review_hold = false
		FROM unnest($2::text[]) WITH ORDINALITY AS n(id, number) WHERE t.id = n.id`, id, in.TrackIDs); err != nil {
		return "", err
	}
	// the album takes the best featured place among those combined
	if _, err := tx.Exec(ctx, `UPDATE albums SET featured_position = (SELECT min(featured_position) FROM albums WHERE id = ANY($1))
		WHERE id = $2`, in.ReleaseIDs, id); err != nil {
		return "", err
	}
	for _, q := range []string{
		`DELETE FROM showcase_items WHERE kind = 'album' AND ref_id = ANY($1)`,
		`DELETE FROM albums WHERE id = ANY($1)`, // only the old rows: nothing they pointed to is removed from storage
	} {
		if _, err := tx.Exec(ctx, q, in.ReleaseIDs); err != nil {
			return "", err
		}
	}
	if in.Public {
		if err := goPublic(ctx, tx, []string{id}); err != nil {
			return "", err
		}
		// followers hear about music that is new to them, not about songs that were already online
		if !wasPublic && s.OnPublish != nil {
			if err := s.notifyLive(ctx, tx, []string{id}, s.OnPublish); err != nil {
				return "", err
			}
		}
	}
	if audit != nil {
		if err := audit(ctx, tx, id, artistID, from); err != nil {
			return "", err
		}
	}
	return id, tx.Commit(ctx)
}
