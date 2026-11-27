// Package artist implements artist profiles, verification requests and the
// storage quota ledger.
package artist

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/review"
)

var (
	ErrNotFound         = errors.New("artist: not found")
	ErrExists           = errors.New("artist: this account already has an artist profile")
	ErrEmailNotVerified = errors.New("artist: verify your email first")
	ErrAlreadyVerified  = errors.New("artist: already verified")
	ErrNotPending       = errors.New("artist: no pending verification request")
)

// ValidationError is returned for bad user input.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Querier is satisfied by *pgxpool.Pool and pgx.Tx.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store owns artist profiles.
type Store struct {
	pool      *pgxpool.Pool
	freeBytes int64
	publicURL string // API base URL, for image links
	// Review: an artist's new or changed profile waits for the team before listeners see it.
	Review bool
}

// NewStore creates the store. freeBytes is the storage every new artist starts with.
func NewStore(pool *pgxpool.Pool, freeBytes int64, publicURL string) *Store {
	return &Store{pool: pool, freeBytes: freeBytes, publicURL: publicURL}
}

// Artist is the API shape of an artist profile.
type Artist struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Bio           string `json:"bio,omitempty"`
	ImageURL      string `json:"imageUrl,omitempty"`
	ImageLargeURL string `json:"imageLargeUrl,omitempty"` // the picture at about 1280 px
	Verified      bool   `json:"verified"`
	FollowerCount int    `json:"followerCount"`
	ReleaseCount  int    `json:"releaseCount"` // public releases

	// Following is whether the person asking follows this artist.
	Following bool `json:"following"`

	// Visible to the owner only.
	Mine             bool   `json:"mine,omitempty"`
	Banned           bool   `json:"banned,omitempty"`       // an administrator suspended this profile
	Verification     string `json:"verification,omitempty"` // unverified | pending | verified | rejected
	VerificationNote string `json:"verificationNote,omitempty"`
}

const artistSelect = `
	SELECT a.id, a.name, a.bio, COALESCE(u.avatar_image_id, a.avatar_image_id), COALESCE(a.image_url, ''), a.verification, a.verification_note,
	       COALESCE(a.owner_user_id::text, ''),
	       (SELECT count(*) FROM library_artists l WHERE l.artist_id = a.id),
	       (SELECT count(*) FROM albums al WHERE al.artist_id = a.id AND al.visibility = 'public'),
	       a.banned_at IS NOT NULL
	FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id`

func (s *Store) scan(row pgx.Row, viewer string) (*Artist, error) {
	var a Artist
	var avatarID *string
	var legacyURL, owner string
	var banned bool
	err := row.Scan(&a.ID, &a.Name, &a.Bio, &avatarID, &legacyURL, &a.Verification, &a.VerificationNote, &owner,
		&a.FollowerCount, &a.ReleaseCount, &banned)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Verified = a.Verification == "verified"
	// An artist's picture is the owner's profile picture (one picture for the
	// person); only old catalog artists without an account use a stored URL.
	a.ImageURL = image.PersonURL(s.publicURL, avatarID, legacyURL)
	a.ImageLargeURL = image.PersonLargeURL(s.publicURL, avatarID)
	if viewer != "" && owner == viewer {
		a.Mine = true
		a.Banned = banned
	} else {
		if banned {
			return nil, ErrNotFound // a suspended profile is gone for everyone but its owner
		}
		a.Verification, a.VerificationNote = "", "" // only the owner sees the workflow state
	}
	return &a, nil
}

// Get returns an artist as seen by viewer (viewer may be empty).
func (s *Store) Get(ctx context.Context, id, viewer string) (*Artist, error) {
	a, err := s.scan(s.pool.QueryRow(ctx, artistSelect+` WHERE a.id = $1`, id), viewer)
	if err != nil {
		return nil, err
	}
	// a new profile nobody has approved is its owner's alone
	var hidden bool
	if err := s.pool.QueryRow(ctx, `SELECT approved_at IS NULL AND owner_user_id::text IS DISTINCT FROM $2 FROM artists WHERE id = $1`, id, viewer).Scan(&hidden); err != nil {
		return nil, err
	}
	if hidden {
		return nil, ErrNotFound
	}
	return a, s.markFollowing(ctx, a, viewer)
}

// GetByOwner returns the user's own artist profile.
func (s *Store) GetByOwner(ctx context.Context, userID string) (*Artist, error) {
	a, err := s.scan(s.pool.QueryRow(ctx, artistSelect+` WHERE a.owner_user_id = $1`, userID), userID)
	if err != nil {
		return nil, err
	}
	return a, s.markFollowing(ctx, a, userID)
}

func (s *Store) markFollowing(ctx context.Context, a *Artist, viewer string) error {
	if viewer == "" {
		return nil
	}
	return s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM library_artists WHERE user_id = $1 AND artist_id = $2)`, viewer, a.ID).Scan(&a.Following)
}

// Follow adds the artist to the user's followed artists (the same list as
// PUT /v1/me/library/artists/{id}). Following twice is fine.
func (s *Store) Follow(ctx context.Context, userID, artistID string) error {
	var followable bool // a suspended profile cannot gain followers
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM artists WHERE id = $1 AND banned_at IS NULL AND approved_at IS NOT NULL)`, artistID).Scan(&followable); err != nil {
		return err
	}
	if !followable {
		return ErrNotFound
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO library_artists (user_id, artist_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, artistID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return ErrNotFound
	}
	return err
}

// Unfollow removes the artist from the user's followed artists (idempotent).
func (s *Store) Unfollow(ctx context.Context, userID, artistID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM library_artists WHERE user_id = $1 AND artist_id = $2`, userID, artistID)
	return err
}

// IDByOwner returns the id of the user's artist profile, or ErrNotFound.
func IDByOwner(ctx context.Context, q Querier, userID string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id FROM artists WHERE owner_user_id = $1`, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

func validName(s string) bool { n := utf8.RuneCountInString(s); return n >= 1 && n <= 100 }

// Create turns the user into an artist. The email must be verified.
func (s *Store) Create(ctx context.Context, userID, name, bio string) (*Artist, error) {
	name, bio = strings.TrimSpace(name), strings.TrimSpace(bio)
	if !validName(name) {
		return nil, &ValidationError{"name must be 1-100 characters"}
	}
	if utf8.RuneCountInString(bio) > 1000 {
		return nil, &ValidationError{"bio must be at most 1000 characters"}
	}
	var verified bool
	if err := s.pool.QueryRow(ctx, `SELECT email_verified_at IS NOT NULL FROM users WHERE id = $1`, userID).Scan(&verified); err != nil {
		return nil, err
	}
	if !verified {
		return nil, ErrEmailNotVerified
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO artists (name, bio, owner_user_id, flagged, approved_at)
		VALUES ($1, $2, $3, EXISTS (SELECT 1 FROM artists WHERE verification = 'verified' AND lower(name) = lower($1)),
		        CASE WHEN $4 THEN NULL ELSE now() END)
		RETURNING id`, name, bio, userID, s.Review).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return nil, ErrExists
	}
	if err != nil {
		return nil, err
	}
	if s.freeBytes > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO storage_grants (artist_id, kind, bytes, note) VALUES ($1, 'free', $2, 'welcome allowance')`,
			id, s.freeBytes); err != nil {
			return nil, err
		}
	}
	if s.Review {
		if err := review.SubmitProfile(ctx, tx, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, id, userID)
}

// GrantWelcome gives an artist the free starting allowance, once in its life (handing a catalog artist to a second
// person does not add another).
func (s *Store) GrantWelcome(ctx context.Context, tx pgx.Tx, artistID string) error {
	if s.freeBytes <= 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO storage_grants (artist_id, kind, bytes, note)
		SELECT $1, 'free', $2, 'welcome allowance' WHERE NOT EXISTS (SELECT 1 FROM storage_grants WHERE artist_id = $1 AND kind = 'free')`,
		artistID, s.freeBytes)
	return err
}

// Patch holds the editable profile fields; nil means "leave unchanged".
type Patch struct {
	Name          *string
	Bio           *string
	AvatarImageID *string // "" removes the avatar
}

// Update edits the owner's profile. Renaming a verified artist removes the
// badge (it must be earned again), so a badge cannot be moved to another name.
func (s *Store) Update(ctx context.Context, id, userID string, p Patch) (*Artist, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var name, bio, verification string
	var approved bool
	err = tx.QueryRow(ctx, `SELECT name, bio, verification, approved_at IS NOT NULL FROM artists
		WHERE id = $1 AND owner_user_id = $2 FOR UPDATE`, id, userID).Scan(&name, &bio, &verification, &approved)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	held := s.Review && approved

	renamed, removedBio := false, false
	var wantName, wantBio *string // what was sent, when it must wait
	if p.Name != nil {
		n := strings.TrimSpace(*p.Name)
		if !validName(n) {
			return nil, &ValidationError{"name must be 1-100 characters"}
		}
		if held {
			wantName = &n
		} else {
			renamed = n != name
			name = n
		}
	}
	if p.Bio != nil {
		b := strings.TrimSpace(*p.Bio)
		if utf8.RuneCountInString(b) > 1000 {
			return nil, &ValidationError{"bio must be at most 1000 characters"}
		}
		switch {
		case held && b != "":
			wantBio = &b
		default: // taking the bio away never waits
			removedBio = held
			bio = b
		}
	}
	if renamed && verification == "verified" {
		verification = "unverified"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE artists SET name = $2, bio = $3, verification = $4, updated_at = now(),
		       flagged = CASE WHEN $5 THEN EXISTS (SELECT 1 FROM artists v WHERE v.verification = 'verified'
		                                           AND lower(v.name) = lower($2) AND v.id <> $1) ELSE flagged END
		WHERE id = $1`, id, name, bio, verification, renamed); err != nil {
		return nil, err
	}
	if held && (wantName != nil || wantBio != nil || removedBio) {
		// a value sent as it already is stops waiting
		if err := review.StageProfile(ctx, tx, id, func(w *review.Profile) {
			if wantName != nil {
				w.Name = wantName
				if *wantName == name {
					w.Name = nil
				}
			}
			if wantBio != nil {
				w.Bio = wantBio
				if *wantBio == bio {
					w.Bio = nil
				}
			}
			if removedBio {
				w.Bio = nil
			}
		}); err != nil {
			return nil, err
		}
	}
	if s.Review && !approved && (p.Name != nil || p.Bio != nil) {
		if err := review.SubmitProfile(ctx, tx, id); err != nil {
			return nil, err
		}
	}
	// Kept for older clients: the artist picture IS the user's profile picture,
	// so this sets the user's avatar (same as PATCH /v1/me).
	if p.AvatarImageID != nil {
		var newAvatar *string
		if *p.AvatarImageID != "" {
			if err := image.CheckOwned(ctx, tx, *p.AvatarImageID, userID, "avatar"); err != nil {
				return nil, &ValidationError{"avatarImageId must be one of your finished avatar images"}
			}
			newAvatar = p.AvatarImageID
		}
		if err := review.SetPicture(ctx, tx, userID, newAvatar, s.Review); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, id, userID)
}

// RequestVerification asks for the verified badge. Asking again while pending is a no-op.
func (s *Store) RequestVerification(ctx context.Context, id, userID, message string) error {
	message = strings.TrimSpace(message)
	if utf8.RuneCountInString(message) > 1000 {
		return &ValidationError{"message must be at most 1000 characters"}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var status string
	err = tx.QueryRow(ctx, `SELECT verification FROM artists WHERE id = $1 AND owner_user_id = $2 FOR UPDATE`, id, userID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	switch status {
	case "verified":
		return ErrAlreadyVerified
	case "pending":
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE artists SET verification = 'pending', verification_message = $2, verification_note = '',
		       verification_requested_at = now(), updated_at = now() WHERE id = $1`, id, message); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ----------------------------------------------------------------------- quota

// Quota is an artist's storage position.
type Quota struct {
	FreeStorageBytes      int64  `json:"freeStorageBytes"`
	PurchasedStorageBytes int64  `json:"purchasedStorageBytes"`
	UsedStorageBytes      int64  `json:"usedStorageBytes"`
	TotalStorageBytes     int64  `json:"totalStorageBytes"`
	RemainingStorageBytes int64  `json:"remainingStorageBytes"`
	Plan                  string `json:"plan"`
	PlanStatus            string `json:"planStatus"`
}

// QuotaFor computes the quota. Free = free + promo grants; purchased = purchases
// and adjustments; expired grants do not count. Used = originals of every
// upload that has not failed. (Transcoded copies and images are ours, not
// the artist's.)
func QuotaFor(ctx context.Context, q Querier, artistID string) (*Quota, error) {
	var out Quota
	err := q.QueryRow(ctx, `
		SELECT
		  COALESCE((SELECT sum(bytes) FROM storage_grants WHERE artist_id = $1 AND kind IN ('free','promo')
		            AND (expires_at IS NULL OR expires_at > now())), 0),
		  COALESCE((SELECT sum(bytes) FROM storage_grants WHERE artist_id = $1 AND kind IN ('purchase','adjustment')
		            AND (expires_at IS NULL OR expires_at > now())), 0),
		  COALESCE((SELECT sum(size_bytes) FROM uploads WHERE artist_id = $1
		            AND status IN ('pending','queued','processing','ready') AND NOT quota_exempt), 0)`, artistID).
		Scan(&out.FreeStorageBytes, &out.PurchasedStorageBytes, &out.UsedStorageBytes)
	if err != nil {
		return nil, err
	}
	out.TotalStorageBytes = max(out.FreeStorageBytes+out.PurchasedStorageBytes, 0)
	out.RemainingStorageBytes = max(out.TotalStorageBytes-out.UsedStorageBytes, 0)
	out.Plan, out.PlanStatus = "free", "active"
	if out.PurchasedStorageBytes > 0 {
		out.Plan = "paid"
	}
	return &out, nil
}

// Quota returns the quota of an artist the user owns.
func (s *Store) Quota(ctx context.Context, id, userID string) (*Quota, error) {
	var owned bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM artists WHERE id = $1 AND owner_user_id = $2)`, id, userID).Scan(&owned); err != nil {
		return nil, err
	}
	if !owned {
		return nil, ErrNotFound
	}
	return QuotaFor(ctx, s.pool, id)
}

// ----------------------------------------------------------------------- admin

// AdminArtist is an artist as the review queue shows it.
type AdminArtist struct {
	Artist
	OwnerEmail  string     `json:"ownerEmail"`
	Message     string     `json:"message,omitempty"`
	RequestedAt *time.Time `json:"requestedAt,omitempty"`
	Flagged     bool       `json:"flagged"`
	Status      string     `json:"status"`
}

// ReviewQueue lists artists waiting for review: filter "pending" (asked for the
// badge, oldest first) or "flagged" (name matches a verified artist).
func (s *Store) ReviewQueue(ctx context.Context, filter string, limit, offset int) ([]*AdminArtist, error) {
	where, order := `a.verification = 'pending'`, `a.verification_requested_at`
	if filter == "flagged" {
		where, order = `a.flagged`, `a.created_at`
	}
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.name, a.bio, COALESCE(u.email, ''), a.verification_message, a.verification_requested_at,
		       a.flagged, a.verification
		FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id
		WHERE `+where+` ORDER BY `+order+`, a.id LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AdminArtist{}
	for rows.Next() {
		var a AdminArtist
		if err := rows.Scan(&a.ID, &a.Name, &a.Bio, &a.OwnerEmail, &a.Message, &a.RequestedAt, &a.Flagged, &a.Status); err != nil {
			return nil, err
		}
		a.Verified = a.Status == "verified"
		out = append(out, &a)
	}
	return out, rows.Err()
}

// Decide approves or rejects a pending verification request.
func (s *Store) Decide(ctx context.Context, id string, approve bool, note string) error {
	note = strings.TrimSpace(note)
	if !approve && (utf8.RuneCountInString(note) < 1 || utf8.RuneCountInString(note) > 500) {
		return &ValidationError{"a rejection needs a reason of 1-500 characters"}
	}
	status := "rejected"
	if approve {
		status, note = "verified", ""
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE artists SET verification = $2, verification_note = $3, flagged = CASE WHEN $2 = 'verified' THEN false ELSE flagged END,
		       updated_at = now()
		WHERE id = $1 AND verification = 'pending'`, id, status, note)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM artists WHERE id = $1)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		return ErrNotPending
	}
	return nil
}

// Grant adds a row to the storage ledger. A repeated reference (a payment id)
// changes nothing and reports created=false, so webhooks can be retried safely.
func (s *Store) Grant(ctx context.Context, id, kind string, bytes int64, note, reference string, expires *time.Time) (created bool, err error) {
	switch kind {
	case "promo", "purchase", "adjustment":
	default:
		return false, &ValidationError{"kind must be promo, purchase or adjustment"}
	}
	if bytes == 0 || (kind != "adjustment" && bytes < 0) {
		return false, &ValidationError{"bytes must be positive (only an adjustment may be negative)"}
	}
	if utf8.RuneCountInString(note) > 500 || utf8.RuneCountInString(reference) > 200 {
		return false, &ValidationError{"note or reference is too long"}
	}
	var ref *string
	if reference != "" {
		ref = &reference
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO storage_grants (artist_id, kind, bytes, note, reference, expires_at)
		SELECT id, $2, $3, $4, $5, $6 FROM artists WHERE id = $1
		ON CONFLICT DO NOTHING`, id, kind, bytes, note, ref, expires)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	// Either the artist does not exist or the reference was already used.
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM artists WHERE id = $1)`, id).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, ErrNotFound
	}
	return false, nil
}
