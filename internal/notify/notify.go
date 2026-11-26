// Package notify decides what the first push notifications say and connects them
// to the events that cause them (a transfer ended, a scheduled release went live).
package notify

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/mailer"
	"github.com/blacmusic/backend/internal/push"
	"github.com/blacmusic/backend/internal/transfer"
	"github.com/blacmusic/backend/internal/upload"
)

// TransferEnded builds the notification for a transfer that completed or failed.
func TransferEnded(e transfer.Ended) (push.Message, string) {
	m := push.Message{Type: push.TypeTransfer, ID: e.TransferID}
	switch {
	case e.Status == transfer.StatusCompleted:
		m.Title = "Import finished"
		switch {
		case e.Done == 1:
			m.Body = "Your YouTube import is done: 1 song is on Blac"
		case e.Done > 1:
			m.Body = fmt.Sprintf("Your YouTube import is done: %d songs are on Blac", e.Done)
		default:
			m.Body = "Your YouTube import is done"
		}
	case e.Failed > 0:
		m.Title = "Import needs attention"
		m.Body = "Your import needs attention: " + push.Plural(e.Failed, "song", "songs") + " failed"
	default:
		m.Title = "Import needs attention"
		m.Body = "Your import needs attention: it could not be finished. Open Blac to try again"
	}
	return m, "transfer:" + e.TransferID + ":" + e.Status
}

// OnTransferEnded is the hook for transfer.Manager.OnEnd.
func OnTransferEnded(db push.DB) func(ctx context.Context, e transfer.Ended) error {
	return func(ctx context.Context, e transfer.Ended) error {
		m, key := TransferEnded(e)
		_, err := push.Enqueue(ctx, db, e.UserID, push.CategoryTransfers, m, key)
		return err
	}
}

// NeedsAudio builds the notification for songs that could not be fetched from YouTube.
func NeedsAudio(e transfer.NeedsAudio) (push.Message, string) {
	return push.Message{
		Type: push.TypeTransfer, ID: e.TransferID, Title: "Import needs attention",
		Body: "Your import needs attention: " + push.Plural(e.Songs, "song needs", "songs need") + " an audio file",
	}, "" // one message per pause; the pause itself happens once
}

// OnNeedsAudio is the hook for transfer.Manager.OnNeedsAudio.
func OnNeedsAudio(db push.DB) func(ctx context.Context, e transfer.NeedsAudio) error {
	return func(ctx context.Context, e transfer.NeedsAudio) error {
		m, key := NeedsAudio(e)
		_, err := push.Enqueue(ctx, db, e.UserID, push.CategoryTransfers, m, key)
		return err
	}
}

// ReleaseLive builds the notification for a scheduled release that went public.
func ReleaseLive(releaseID, title string) (push.Message, string) {
	return push.Message{Title: "Your release is live", Body: "'" + title + "' is live", Type: push.TypeRelease, ID: releaseID}, "release-live:" + releaseID
}

// OnReleaseLive is the hook for release.Store.OnLive. It queues the push and the email inside the
// transaction that makes the scheduled release public.
func OnReleaseLive(ctx context.Context, tx pgx.Tx, releaseID, title, ownerUserID string) error {
	m, key := ReleaseLive(releaseID, title)
	if _, err := push.Enqueue(ctx, tx, ownerUserID, push.CategoryReleases, m, key); err != nil {
		return err
	}
	return emailLive(ctx, tx, ownerUserID, title, true)
}

// OnReleasePublished is the hook for release.Store.OnPublish: the artist published a release right
// away, so they get the confirmation email (no push: they are in the app).
func OnReleasePublished(ctx context.Context, tx pgx.Tx, _, title, ownerUserID string) error {
	return emailLive(ctx, tx, ownerUserID, title, false)
}

// oneLine keeps titles from breaking an email's subject.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// signoff ends every release email.
const signoff = "\n\nThank you for sharing your music.\n\nThe Blac Music team"

func emailLive(ctx context.Context, db mailer.DB, owner, title string, scheduled bool) error {
	title = oneLine(title)
	lead := "\"" + title + "\" is now live on Blac Music."
	if scheduled {
		lead = "\"" + title + "\" went live on Blac Music, as you scheduled."
	}
	body := lead + " Listeners can find it by searching for it or opening your artist page." + signoff
	_, err := mailer.EnqueueRelease(ctx, db, owner, "Your release is live: "+title, body, "")
	return err
}

// ReleaseProcessed builds the notification for a release whose audio is all processed.
func ReleaseProcessed(e upload.ReadyEvent) (push.Message, string) {
	return push.Message{
		Title: "Your audio is ready",
		Body:  "'" + oneLine(e.ReleaseTitle) + "': " + push.Plural(e.Tracks, "track is", "tracks are") + " ready",
		Type:  push.TypeRelease, ID: e.ReleaseID,
	}, "release-ready:" + e.UploadID
}

// OnReleaseProcessed is the hook for upload.Store.OnReady. It queues the push and the email inside the
// transaction that finishes the last file of the release.
func OnReleaseProcessed(ctx context.Context, tx pgx.Tx, e upload.ReadyEvent) error {
	m, key := ReleaseProcessed(e)
	if _, err := push.Enqueue(ctx, tx, e.OwnerUserID, push.CategoryReleases, m, key); err != nil {
		return err
	}
	title := oneLine(e.ReleaseTitle)
	body := push.Plural(e.Tracks, "track", "tracks") + " in \"" + title + "\" finished processing and "
	if e.Tracks == 1 {
		body = "The track in \"" + title + "\" finished processing and is "
	} else {
		body += "are "
	}
	if e.Live {
		body += "now playing for your listeners."
	} else {
		body += "ready. Open Blac Music to review it and publish when you are ready."
	}
	body += signoff
	_, err := mailer.EnqueueRelease(ctx, tx, e.OwnerUserID, "Your music is ready: "+title, body, key)
	return err
}
