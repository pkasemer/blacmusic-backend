package review

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/mailer"
	"github.com/blacmusic/backend/internal/push"
)

// pushProfile is the type of a notification about the artist profile: the app opens Studio.
const pushProfile = "artist_profile"

// Profile is the changes waiting for an artist's profile; nil is unchanged. A new profile has none: what was sent is
// on the (hidden) profile itself.
type Profile struct {
	Name          *string `json:"name,omitempty"`
	Bio           *string `json:"bio,omitempty"`
	AvatarImageID *string `json:"avatarImageId,omitempty"`
}

// Empty says whether nothing is waiting.
func (p *Profile) Empty() bool { return p.Name == nil && p.Bio == nil && p.AvatarImageID == nil }

// Labels names the waiting changes in words.
func (p *Profile) Labels() []string {
	out := []string{}
	if p.Name != nil {
		out = append(out, "Name")
	}
	if p.AvatarImageID != nil {
		out = append(out, "Photo")
	}
	if p.Bio != nil {
		out = append(out, "Bio")
	}
	return out
}

// ProfileRow is one submission about a profile.
type ProfileRow struct {
	ID, ArtistID, Kind, Status, Reason string
	Revision                           int
	SubmittedAt                        time.Time
	DecidedAt                          *time.Time
	Changes                            Profile
}

const profileColumns = `id, artist_id, kind, status, reason, revision, submitted_at, decided_at, changes`

func scanProfile(row pgx.Row) (*ProfileRow, error) {
	var r ProfileRow
	var raw []byte
	err := row.Scan(&r.ID, &r.ArtistID, &r.Kind, &r.Status, &r.Reason, &r.Revision, &r.SubmittedAt, &r.DecidedAt, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, json.Unmarshal(raw, &r.Changes)
}

// GetProfile reads one submission; nil when there is none.
func GetProfile(ctx context.Context, db DB, id string) (*ProfileRow, error) {
	return scanProfile(db.QueryRow(ctx, `SELECT `+profileColumns+` FROM artist_reviews WHERE id = $1`, id))
}

// LatestProfile is the artist's last submission about the profile; nil when there never was one.
func LatestProfile(ctx context.Context, db DB, artistID string) (*ProfileRow, error) {
	return scanProfile(db.QueryRow(ctx, `SELECT `+profileColumns+` FROM artist_reviews WHERE artist_id = $1 ORDER BY seq DESC LIMIT 1`, artistID))
}

func pendingProfile(ctx context.Context, db DB, artistID string) (*ProfileRow, error) {
	return scanProfile(db.QueryRow(ctx, `SELECT `+profileColumns+` FROM artist_reviews WHERE artist_id = $1 AND status = 'pending'`, artistID))
}

// SubmitProfile sends a new, still hidden profile to be checked, or notes that it changed while it waits. The caller
// holds the artist's row lock.
func SubmitProfile(ctx context.Context, db DB, artistID string) error {
	tag, err := db.Exec(ctx, `UPDATE artist_reviews SET revision = revision + 1 WHERE artist_id = $1 AND status = 'pending'`, artistID)
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}
	var id string
	if err := db.QueryRow(ctx, `INSERT INTO artist_reviews (artist_id, kind) VALUES ($1, 'new') RETURNING id`, artistID).Scan(&id); err != nil {
		return err
	}
	return tellProfile(ctx, db, artistID, "profile-sent:"+id, "sent", "", "")
}

// StageProfile adds to (or takes from) the changes waiting for an approved profile. The caller holds the artist's row
// lock.
func StageProfile(ctx context.Context, db DB, artistID string, change func(*Profile)) error {
	r, err := pendingProfile(ctx, db, artistID)
	if err != nil {
		return err
	}
	if r == nil {
		var p Profile
		change(&p)
		if p.Empty() {
			return nil
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		var id string
		if err := db.QueryRow(ctx, `INSERT INTO artist_reviews (artist_id, kind, changes) VALUES ($1, 'update', $2) RETURNING id`,
			artistID, raw).Scan(&id); err != nil {
			return err
		}
		return tellProfile(ctx, db, artistID, "profile-sent:"+id, "sent-update", "", "")
	}
	change(&r.Changes)
	if r.Kind == "update" && r.Changes.Empty() {
		return CloseProfile(ctx, db, r.ID, "withdrawn", "", "")
	}
	raw, err := json.Marshal(r.Changes)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `UPDATE artist_reviews SET changes = $2, revision = revision + 1 WHERE id = $1`, r.ID, raw)
	return err
}

// CloseProfile ends a submission about a profile. A waiting picture goes with it unless it was approved (the caller
// has then made it the picture).
func CloseProfile(ctx context.Context, db DB, id, status, by, reason string) error {
	var who *string
	if by != "" {
		who = &by
	}
	if _, err := db.Exec(ctx, `UPDATE artist_reviews SET status = $2, decided_at = now(), decided_by = $3, reason = $4 WHERE id = $1`,
		id, status, who, reason); err != nil {
		return err
	}
	_, err := db.Exec(ctx, `UPDATE users SET next_avatar_image_id = NULL WHERE id =
		(SELECT a.owner_user_id FROM artist_reviews r JOIN artists a ON a.id = r.artist_id WHERE r.id = $1)`, id)
	return err
}

// SetPicture changes a person's profile picture (avatar nil removes it). For an artist whose publishing waits for
// review, a new picture on an approved profile is shown to listeners only once approved; removing one never waits.
func SetPicture(ctx context.Context, db DB, userID string, avatar *string, review bool) error {
	var artistID string
	var approved bool
	err := db.QueryRow(ctx, `SELECT id, approved_at IS NOT NULL FROM artists WHERE owner_user_id::text = $1 FOR UPDATE`, userID).Scan(&artistID, &approved)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	artist := err == nil
	if artist && review && approved && avatar != nil {
		var same bool
		if err := db.QueryRow(ctx, `SELECT COALESCE(avatar_image_id, '') = $2 FROM users WHERE id::text = $1`, userID, *avatar).Scan(&same); err != nil {
			return err
		}
		next := avatar
		if same {
			next = nil
		}
		if _, err := db.Exec(ctx, `UPDATE users SET next_avatar_image_id = $2 WHERE id::text = $1`, userID, next); err != nil {
			return err
		}
		return StageProfile(ctx, db, artistID, func(p *Profile) { p.AvatarImageID = next })
	}
	if _, err := db.Exec(ctx, `UPDATE users SET avatar_image_id = $2, next_avatar_image_id = NULL WHERE id::text = $1`, userID, avatar); err != nil {
		return err
	}
	if !artist || !review {
		return nil
	}
	if !approved { // the hidden profile changed: what the team saw is out of date, and a refused one is sent again
		return SubmitProfile(ctx, db, artistID)
	}
	return StageProfile(ctx, db, artistID, func(p *Profile) { p.AvatarImageID = nil })
}

// TellProfileDecided tells the artist what the team decided about their profile.
func TellProfileDecided(ctx context.Context, db DB, artistID, reviewID, kind string, approved bool, reason string) error {
	what := "approved"
	if !approved {
		what = "rejected"
	}
	return tellProfile(ctx, db, artistID, "profile-"+what+":"+reviewID, what, kind, reason)
}

func tellProfile(ctx context.Context, db DB, artistID, key, what, kind, reason string) error {
	var owner *string
	var name string
	if err := db.QueryRow(ctx, `SELECT owner_user_id::text, name FROM artists WHERE id = $1`, artistID).Scan(&owner, &name); err != nil || owner == nil {
		return err
	}
	var subject, body, short string
	switch what {
	case "sent":
		subject, body = "We're verifying your artist profile", "Thank you. Our team is checking the artist profile \""+name+"\" before listeners see it. You can keep working in Studio meanwhile."
	case "sent-update":
		subject, body = "We're verifying your profile changes", "Thank you. Our team is checking your artist profile changes. Listeners keep seeing your current profile until then."
	case "approved":
		subject, short = "Your artist profile is approved", "Your artist profile is approved"
		body = "Good news: your artist profile \"" + name + "\" passed our check and listeners can now see it."
		if kind == "update" {
			subject, short = "Your profile changes are approved", "Your profile changes are live"
			body = "Good news: your artist profile changes passed our check and listeners now see them."
		}
	default:
		subject, short = "Your artist profile needs changes", reason
		body = "Our team could not approve your artist profile yet.\n\nReason: " + reason + "\n\nOpen Blac Music, fix this and save it again."
	}
	if _, err := mailer.EnqueueRelease(ctx, db, *owner, subject, body+signoff, key); err != nil {
		return err
	}
	if short == "" {
		return nil
	}
	_, err := push.Enqueue(ctx, db, *owner, push.CategoryReleases, push.Message{Title: subject, Body: short, Type: pushProfile, ID: artistID}, key)
	return err
}
