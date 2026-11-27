package review

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/mailer"
	"github.com/blacmusic/backend/internal/push"
)

// The artist hears by email when something is sent, and by email and notification when it is decided. Their release
// switches apply. A catalog artist with no account is told nothing.

const signoff = "\n\nThe Blac Music team"

// who is the account behind a release, and its title; ok is false when there is none.
func who(ctx context.Context, db DB, releaseID string) (owner, title string, ok bool, err error) {
	var o *string
	err = db.QueryRow(ctx, `SELECT a.owner_user_id::text, al.title FROM albums al JOIN artists a ON a.id = al.artist_id WHERE al.id = $1`,
		releaseID).Scan(&o, &title)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && o == nil) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return *o, strings.Join(strings.Fields(title), " "), true, nil
}

func tell(ctx context.Context, db DB, owner, releaseID, key, subject, body string, alsoPush bool, short string) error {
	if _, err := mailer.EnqueueRelease(ctx, db, owner, subject, body+signoff, key); err != nil {
		return err
	}
	if !alsoPush {
		return nil
	}
	_, err := push.Enqueue(ctx, db, owner, push.CategoryReleases, push.Message{Title: subject, Body: short, Type: push.TypeRelease, ID: releaseID}, key)
	return err
}

func tellSubmitted(ctx context.Context, db DB, releaseID, reviewID, kind string) error {
	owner, title, ok, err := who(ctx, db, releaseID)
	if !ok {
		return err
	}
	body := "Thank you. Our team is checking \"" + title + "\" before it goes live. We will email you when we have looked at it."
	if kind == "update" {
		body = "Thank you. Our team is checking your changes to \"" + title + "\". Listeners keep seeing the current version until then. We will email you when we have looked at them."
	}
	return tell(ctx, db, owner, releaseID, "review-sent:"+reviewID, "We're verifying "+title, body, false, "")
}

// TellApproved says a submission passed. goesLive is set for a release that is scheduled.
func TellApproved(ctx context.Context, db DB, releaseID, reviewID, kind string, goesLive *time.Time) error {
	owner, title, ok, err := who(ctx, db, releaseID)
	if !ok {
		return err
	}
	subject, short := title+" is approved", "'"+title+"' is live"
	body := "Good news: \"" + title + "\" passed our check and is now live on Blac Music."
	switch {
	case kind == "update":
		subject, short = "Your changes to "+title+" are approved", "Your changes to '"+title+"' are live"
		body = "Good news: your changes to \"" + title + "\" passed our check and listeners now see them."
	case goesLive != nil:
		when := goesLive.In(eastAfrica).Format("2 January 2006 at 15:04") + " (East Africa Time)"
		short = "'" + title + "' goes live on " + when
		body = "Good news: \"" + title + "\" passed our check. It goes live on " + when + "."
	}
	return tell(ctx, db, owner, releaseID, "review-approved:"+reviewID, subject, body, true, short)
}

// TellRejected says why a submission did not pass.
func TellRejected(ctx context.Context, db DB, releaseID, reviewID, kind, reason string) error {
	owner, title, ok, err := who(ctx, db, releaseID)
	if !ok {
		return err
	}
	what := "\"" + title + "\""
	if kind == "update" {
		what = "your changes to " + what
	}
	body := "Our team could not approve " + what + " yet.\n\nReason: " + reason + "\n\nOpen Blac Music, fix this and send it again."
	return tell(ctx, db, owner, releaseID, "review-rejected:"+reviewID, title+" needs changes", body, true, reason)
}

// eastAfrica is Uganda's time, which has no daylight saving.
var eastAfrica = time.FixedZone("EAT", 3*60*60)
