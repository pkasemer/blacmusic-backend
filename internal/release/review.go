package release

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/credits"
	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/lyrics"
	"github.com/blacmusic/backend/internal/review"
)

var (
	// ErrNotPending: nothing is waiting (it was withdrawn or already decided).
	ErrNotPending = errors.New("release: nothing is waiting for review")
	// ErrReviewChanged: what waits changed after the administrator opened it.
	ErrReviewChanged = errors.New("release: what is waiting changed since it was opened")
	// ErrProfilePending: the artist's new profile has to be approved before their release can be.
	ErrProfilePending = errors.New("release: approve the artist profile first")
)

// Withdraw takes back what the artist has waiting: a new release becomes a plain draft again, waiting changes are
// dropped.
func (s *Store) Withdraw(ctx context.Context, id, userID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM albums al JOIN artists a ON a.id = al.artist_id
		WHERE al.id = $1 AND a.owner_user_id = $2 FOR UPDATE OF al`, id, userID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	p, err := review.Pending(ctx, tx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return ErrNotPending
	}
	if err := review.Close(ctx, tx, p.ID, "withdrawn", "", ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Decide is the team's answer to a submission, on the revision the administrator saw. Approving a new release makes
// it public, or scheduled when the time asked for is still ahead; approving changes applies them. Rejecting changes
// nothing that anyone sees. audit runs in the same transaction.
func (s *Store) Decide(ctx context.Context, reviewID string, revision int, adminID string, approve bool, reason string,
	audit func(ctx context.Context, tx pgx.Tx, r *review.Row, title string) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	r, err := review.Get(ctx, tx, reviewID)
	if err != nil {
		return err
	}
	if r == nil {
		return ErrNotFound
	}
	// the release's lock first, as every change to it takes it, then what is waiting as it is now
	var title, rtype, visibility string
	var takenDown, banned bool
	if err := tx.QueryRow(ctx, `
		SELECT al.title, al.release_type, al.visibility, al.taken_down_at IS NOT NULL, COALESCE(a.banned_at IS NOT NULL, false)
		FROM albums al LEFT JOIN artists a ON a.id = al.artist_id WHERE al.id = $1 FOR UPDATE OF al`, r.ReleaseID).
		Scan(&title, &rtype, &visibility, &takenDown, &banned); err != nil {
		return err
	}
	if r, err = review.Get(ctx, tx, reviewID); err != nil {
		return err
	}
	switch {
	case r == nil || r.Status != "pending":
		return ErrNotPending
	case r.Revision != revision:
		return ErrReviewChanged
	}
	if !approve {
		if err := review.Close(ctx, tx, r.ID, "rejected", adminID, reason); err != nil {
			return err
		}
		if err := review.TellRejected(ctx, tx, r.ReleaseID, r.ID, r.Kind, reason); err != nil {
			return err
		}
	} else {
		if takenDown {
			return ErrTakenDown
		}
		if banned {
			return ErrArtistBanned
		}
		var hidden bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT a.approved_at IS NULL FROM artists a JOIN albums al ON al.artist_id = a.id WHERE al.id = $1), false)`,
			r.ReleaseID).Scan(&hidden); err != nil {
			return err
		}
		if hidden {
			return ErrProfilePending
		}
		var goesLive *time.Time
		if r.Kind == "new" {
			goesLive, err = s.approveNew(ctx, tx, r, rtype)
		} else {
			err = s.applyChanges(ctx, tx, r.ReleaseID, r.Changes)
			if err == nil {
				err = releaseHeld(ctx, tx, r.ReleaseID, r.Changes.NewTracks, rtype, visibility)
			}
		}
		if err != nil {
			return err
		}
		if err := review.Close(ctx, tx, r.ID, "approved", adminID, ""); err != nil {
			return err
		}
		// one message for this moment: a scheduled release is announced again when it goes live
		if err := review.TellApproved(ctx, tx, r.ReleaseID, r.ID, r.Kind, goesLive); err != nil {
			return err
		}
	}
	if audit != nil {
		if err := audit(ctx, tx, r, title); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// publishable checks that a release has what it needs to go online.
func publishable(ctx context.Context, tx pgx.Tx, id, rtype string) error {
	var tracks, inflight int
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM tracks WHERE album_id = $1),
		(SELECT count(*) FROM uploads WHERE release_id = $1 AND status IN ('pending','queued','processing'))`, id).
		Scan(&tracks, &inflight); err != nil {
		return err
	}
	switch {
	case inflight > 0:
		return ErrUploadsPending
	case tracks == 0:
		return ErrNoTracks
	case rtype == "single" && tracks != 1:
		return &ValidationError{"a single must have exactly one track (make it an album instead)"}
	}
	return nil
}

func (s *Store) approveNew(ctx context.Context, tx pgx.Tx, r *review.Row, rtype string) (*time.Time, error) {
	if err := publishable(ctx, tx, r.ReleaseID, rtype); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE tracks SET review_hold = false WHERE album_id = $1`, r.ReleaseID); err != nil {
		return nil, err
	}
	if r.PublishAt != nil && r.PublishAt.After(time.Now().Add(time.Minute)) {
		_, err := tx.Exec(ctx, `UPDATE albums SET visibility = 'scheduled', publish_at = $2, updated_at = now() WHERE id = $1`,
			r.ReleaseID, r.PublishAt.UTC())
		return r.PublishAt, err
	}
	return nil, goPublic(ctx, tx, []string{r.ReleaseID})
}

// releaseHeld lets the approved new songs of an online release be heard.
func releaseHeld(ctx context.Context, tx pgx.Tx, releaseID string, ids []string, rtype, visibility string) error {
	if len(ids) == 0 {
		return nil
	}
	if rtype == "single" {
		var tracks int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tracks WHERE album_id = $1`, releaseID).Scan(&tracks); err != nil {
			return err
		}
		if tracks != 1 {
			return &ValidationError{"a single must have exactly one track: reject this, or make the release an album first"}
		}
	}
	_, err := tx.Exec(ctx, `UPDATE tracks SET review_hold = false, visibility = CASE WHEN $3 = 'public' THEN 'public' ELSE visibility END
		WHERE album_id = $1 AND id = ANY($2)`, releaseID, ids, visibility)
	return err
}

// applyChanges puts waiting changes onto the release. Songs that have since left it are skipped.
func (s *Store) applyChanges(ctx context.Context, tx pgx.Tx, releaseID string, c review.Changes) error {
	for key, column := range map[string]string{"title": "title", "description": "description", "genre": "genre"} {
		if v, ok := c.Release[key]; ok {
			if _, err := tx.Exec(ctx, `UPDATE albums SET `+column+` = $2, updated_at = now() WHERE id = $1`, releaseID, v); err != nil {
				return err
			}
		}
	}
	if v, ok := c.Release["releaseDate"]; ok {
		if _, err := tx.Exec(ctx, `UPDATE albums SET release_date = NULLIF($2, '')::date, updated_at = now() WHERE id = $1`, releaseID, v); err != nil {
			return err
		}
	}
	if v, ok := c.Release["coverImageId"]; ok {
		var cover, url *string
		if v != "" {
			u := image.URL(s.publicURL, v, image.SizeMedium)
			cover, url = &v, &u
		}
		if _, err := tx.Exec(ctx, `UPDATE albums SET cover_image_id = $2, artwork_url = $3, updated_at = now() WHERE id = $1`, releaseID, cover, url); err != nil {
			return err
		}
	}
	for id, t := range c.Tracks {
		if _, err := tx.Exec(ctx, `UPDATE tracks SET title = COALESCE($3, title), explicit = COALESCE($4, explicit), genre = COALESCE($5, genre),
			track_number = CASE WHEN $6::int IS NULL THEN track_number ELSE NULLIF($6::int, 0) END WHERE id = $1 AND album_id = $2`,
			id, releaseID, t.Title, t.Explicit, t.Genre, t.TrackNumber); err != nil {
			return err
		}
	}
	if len(c.Credits) > 0 {
		var list []credits.Input
		if err := json.Unmarshal(c.Credits, &list); err != nil {
			return err
		}
		if err := credits.Write(ctx, tx, releaseID, list); err != nil {
			return err
		}
	}
	for id, raw := range c.Lyrics {
		var here bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tracks WHERE id = $1 AND album_id = $2)`, id, releaseID).Scan(&here); err != nil {
			return err
		}
		if !here {
			continue
		}
		var l lyrics.Lyrics
		if err := json.Unmarshal(raw, &l); err != nil {
			return err
		}
		if err := lyrics.Save(ctx, tx, id, &l); err != nil {
			return err
		}
	}
	return nil
}

// settle puts what was waiting onto a release that is going back to draft: nobody sees a draft, and publishing it
// again sends the whole of it to be checked.
func (s *Store) settle(ctx context.Context, tx pgx.Tx, releaseID string) error {
	p, err := review.Pending(ctx, tx, releaseID)
	if err != nil || p == nil {
		return err
	}
	if p.Kind == "update" {
		if err := s.applyChanges(ctx, tx, releaseID, p.Changes); err != nil {
			return err
		}
	}
	return review.Close(ctx, tx, p.ID, "withdrawn", "", "")
}

// ownerReview adds to the owner's view of a release what is waiting or refused, and shows waiting changes as the
// owner last made them, so editing goes on from there. Everyone else sees the approved release.
func (s *Store) ownerReview(ctx context.Context, r *Release) error {
	if !r.Mine {
		return nil
	}
	r.ReviewRequired = s.Review
	info, row, err := review.ForOwner(ctx, s.pool, r.ID)
	if err != nil || info == nil {
		return err
	}
	r.Review = info
	if row.Status != "pending" || row.Kind != "update" {
		return nil
	}
	c := row.Changes
	for key, field := range map[string]*string{"title": &r.Title, "description": &r.Description, "genre": &r.Genre, "releaseDate": &r.ReleaseDate} {
		if v, ok := c.Release[key]; ok {
			*field = v
		}
	}
	if v, ok := c.Release["coverImageId"]; ok {
		r.ArtworkURL = ""
		if v != "" {
			r.ArtworkURL = image.URL(s.publicURL, v, image.SizeMedium)
		}
		r.ArtworkLargeURL = image.LargeURL(r.ArtworkURL)
	}
	for i := range r.Tracks {
		if t, ok := c.Tracks[r.Tracks[i].ID]; ok {
			applyToView(&r.Tracks[i], t)
		}
	}
	return nil
}

func applyToView(v *TrackView, t review.TrackChange) {
	if t.Title != nil {
		v.Title = *t.Title
	}
	if t.Explicit != nil {
		v.Explicit = *t.Explicit
	}
	if t.Genre != nil {
		v.Genre = *t.Genre
	}
	if t.TrackNumber != nil {
		v.TrackNumber = t.TrackNumber
		if *t.TrackNumber == 0 {
			v.TrackNumber = nil
		}
	}
}
