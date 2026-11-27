package artist

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/review"
)

var (
	// ErrNoReview: nothing is waiting for review.
	ErrNoReview = errors.New("artist: nothing is waiting for review")
	// ErrBanned: the profile is suspended.
	ErrBanned = errors.New("artist: this artist is banned")
	// ErrNotWithdrawable: a new profile is not taken back.
	ErrNotWithdrawable = errors.New("artist: a new profile cannot be withdrawn")
	// ErrReviewChanged: what waits changed after the administrator opened it.
	ErrReviewChanged = errors.New("artist: what is waiting changed since it was opened")
)

// OwnerReview is what the artist is told about their profile when something waits or was refused.
type OwnerReview struct {
	Status  string   `json:"status"` // pending | rejected
	Kind    string   `json:"kind"`   // new | update
	Reason  string   `json:"reason,omitempty"`
	Changes []string `json:"changes"`
}

// PendingProfile is the profile fields waiting to be approved.
type PendingProfile struct {
	Name     *string `json:"name,omitempty"`
	Bio      *string `json:"bio,omitempty"`
	ImageURL string  `json:"imageUrl,omitempty"`
}

// ReviewOf is the owner's view of their profile's review; nils when nothing waits or was refused.
func (s *Store) ReviewOf(ctx context.Context, artistID string) (*OwnerReview, *PendingProfile, error) {
	r, err := review.LatestProfile(ctx, s.pool, artistID)
	if err != nil || r == nil || (r.Status != "pending" && r.Status != "rejected") {
		return nil, nil, err
	}
	info := &OwnerReview{Status: r.Status, Kind: r.Kind, Reason: r.Reason, Changes: r.Changes.Labels()}
	if r.Status != "pending" || r.Kind != "update" {
		return info, nil, nil
	}
	p := &PendingProfile{Name: r.Changes.Name, Bio: r.Changes.Bio}
	if id := r.Changes.AvatarImageID; id != nil {
		p.ImageURL = image.URL(s.publicURL, *id, image.SizeMedium)
	}
	return info, p, nil
}

// WithdrawProfile drops the profile changes waiting for review.
func (s *Store) WithdrawProfile(ctx context.Context, userID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM artists WHERE owner_user_id::text = $1 FOR UPDATE`, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	r, err := review.LatestProfile(ctx, tx, id)
	switch {
	case err != nil:
		return err
	case r == nil || r.Status != "pending":
		return ErrNoReview
	case r.Kind == "new":
		return ErrNotWithdrawable
	}
	if err := review.CloseProfile(ctx, tx, r.ID, "withdrawn", "", ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DecideProfile is the team's answer to a profile submission, on the revision the administrator saw. audit runs in
// the same transaction.
func (s *Store) DecideProfile(ctx context.Context, reviewID string, revision int, adminID string, approve bool, reason string,
	audit func(ctx context.Context, tx pgx.Tx, r *review.ProfileRow, name string) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	r, err := review.GetProfile(ctx, tx, reviewID)
	if err != nil {
		return err
	}
	if r == nil {
		return ErrNotFound
	}
	var name, verification string
	var banned bool
	var owner *string
	if err := tx.QueryRow(ctx, `SELECT name, verification, banned_at IS NOT NULL, owner_user_id::text FROM artists WHERE id = $1 FOR UPDATE`, r.ArtistID).
		Scan(&name, &verification, &banned, &owner); err != nil {
		return err
	}
	if r, err = review.GetProfile(ctx, tx, reviewID); err != nil {
		return err
	}
	switch {
	case r == nil || r.Status != "pending":
		return ErrNoReview
	case r.Revision != revision:
		return ErrReviewChanged
	case approve && banned:
		return ErrBanned
	}
	if approve {
		c := r.Changes
		renamed := c.Name != nil && *c.Name != name
		if renamed {
			name = *c.Name
			if verification == "verified" { // the badge was for the old name
				verification = "unverified"
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE artists SET name = $2, bio = COALESCE($3, bio), verification = $4, approved_at = COALESCE(approved_at, now()), updated_at = now(),
			       flagged = CASE WHEN $5 THEN EXISTS (SELECT 1 FROM artists v WHERE v.verification = 'verified'
			                                           AND lower(v.name) = lower($2) AND v.id <> $1) ELSE flagged END
			WHERE id = $1`, r.ArtistID, name, c.Bio, verification, renamed); err != nil {
			return err
		}
		if c.AvatarImageID != nil && owner != nil {
			if _, err := tx.Exec(ctx, `UPDATE users SET avatar_image_id = $2 WHERE id::text = $1`, *owner, *c.AvatarImageID); err != nil {
				return err
			}
		}
	}
	status := "approved"
	if !approve {
		status = "rejected"
	}
	if err := review.CloseProfile(ctx, tx, r.ID, status, adminID, reason); err != nil {
		return err
	}
	if err := review.TellProfileDecided(ctx, tx, r.ArtistID, r.ID, r.Kind, approve, reason); err != nil {
		return err
	}
	if audit != nil {
		if err := audit(ctx, tx, r, name); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
