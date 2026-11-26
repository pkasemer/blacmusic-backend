package notify

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/blobtest"
	"github.com/blacmusic/backend/internal/push"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/testutil"
	"github.com/blacmusic/backend/internal/upload"
)

type mail struct{ subject, body string }

func emails(t *testing.T, pool *pgxpool.Pool, user string) []mail {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT subject, body FROM email_messages WHERE user_id = $1 ORDER BY id`, user)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []mail
	for rows.Next() {
		var m mail
		if err := rows.Scan(&m.subject, &m.body); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func pushes(t *testing.T, pool *pgxpool.Pool, user string) []map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT title, body, data->>'type', data->>'id' FROM push_messages WHERE user_id = $1 ORDER BY id`, user)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var a, b, c, d string
		rows.Scan(&a, &b, &c, &d) //nolint:errcheck
		out = append(out, map[string]string{"title": a, "body": b, "type": c, "id": d})
	}
	return out
}

func TestPublishingSendsAConfirmationEmailAndNoPush(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SerializeOutbox(t, pool)
	ctx := context.Background()
	store := release.NewStore(pool, blobtest.New(), "http://api.test")
	store.OnPublish = OnReleasePublished
	ps := push.NewStore(pool)

	user := testutil.User(t, pool, "user", true)
	artist := testutil.Artist(t, pool, user, testutil.Unique("art"), 1<<30)
	ps.Register(ctx, user, "ExponentPushToken[publishaaaabbbb]", "ios", "") //nolint:errcheck
	publish := func(title string) {
		t.Helper()
		rel := testutil.Release(t, pool, artist, title, "single", "draft")
		testutil.ReadyTrack(t, pool, user, artist, rel, title, 1, "draft")
		if _, err := store.Publish(ctx, rel, user, nil); err != nil {
			t.Fatal(err)
		}
	}

	publish("Mukene")
	got := emails(t, pool, user)
	if len(got) != 1 || got[0].subject != `Your release is live: Mukene` || !strings.Contains(got[0].body, `"Mukene" is now live on Blac Music.`) {
		t.Fatalf("emails = %+v", got)
	}
	if n := len(pushes(t, pool, user)); n != 0 {
		t.Fatalf("a push for a release the artist just published themself: %d", n)
	}

	// publishing something that is already public changes nothing, so it sends nothing
	rel := testutil.Release(t, pool, artist, "Again", "single", "public")
	if _, err := store.Publish(ctx, rel, user, nil); err != nil {
		t.Fatal(err)
	}
	if len(emails(t, pool, user)) != 1 {
		t.Fatal("publishing a public release sent another email")
	}

	// the email switch is separate from the push switches
	off := false
	ps.SetSettings(ctx, user, push.SettingsPatch{ReleaseEmails: &off}) //nolint:errcheck
	publish("Quiet")
	if len(emails(t, pool, user)) != 1 {
		t.Fatal("an email was queued although release emails are off")
	}
	on := true
	ps.SetSettings(ctx, user, push.SettingsPatch{ReleaseEmails: &on, Releases: &off}) //nolint:errcheck
	publish("Loud")
	if len(emails(t, pool, user)) != 2 {
		t.Fatal("switching release pushes off must not stop the emails")
	}

	// an address that was never verified gets nothing
	stranger := testutil.User(t, pool, "user", false)
	sa := testutil.Artist(t, pool, stranger, testutil.Unique("art"), 1<<30)
	rel = testutil.Release(t, pool, sa, "Unverified", "single", "draft")
	testutil.ReadyTrack(t, pool, stranger, sa, rel, "u", 1, "draft")
	if _, err := store.Publish(ctx, rel, stranger, nil); err != nil {
		t.Fatal(err)
	}
	if len(emails(t, pool, stranger)) != 0 {
		t.Fatal("emailed an unverified address")
	}
}

func TestScheduledReleaseGoingLiveSendsEmailAndPush(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SerializeOutbox(t, pool)
	ctx := context.Background()
	store := release.NewStore(pool, blobtest.New(), "http://api.test")
	store.OnLive = OnReleaseLive
	user := testutil.User(t, pool, "user", true)
	artist := testutil.Artist(t, pool, user, testutil.Unique("art"), 1<<30)
	push.NewStore(pool).Register(ctx, user, "ExponentPushToken[liveaaaabbbbcc]", "ios", "") //nolint:errcheck
	rel := testutil.Release(t, pool, artist, "Blessed", "single", "scheduled")
	testutil.ReadyTrack(t, pool, user, artist, rel, "t", 1, "draft")
	pool.Exec(ctx, `UPDATE albums SET publish_at = now() - interval '1 minute' WHERE id = $1`, rel) //nolint:errcheck

	if _, err := store.PublishDue(ctx); err != nil {
		t.Fatal(err)
	}
	got := emails(t, pool, user)
	if len(got) != 1 || got[0].subject != "Your release is live: Blessed" || !strings.Contains(got[0].body, "as you scheduled") {
		t.Fatalf("emails = %+v", got)
	}
	if p := pushes(t, pool, user); len(p) != 1 || p[0]["type"] != "release" || p[0]["id"] != rel {
		t.Fatalf("pushes = %v", p)
	}
	store.PublishDue(ctx) //nolint:errcheck
	if len(emails(t, pool, user)) != 1 {
		t.Fatal("a second run sent another email")
	}
}

func TestProcessedAudioQueuesAPushAndAnEmail(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SerializeOutbox(t, pool)
	ctx := context.Background()
	user := testutil.User(t, pool, "user", true)
	push.NewStore(pool).Register(ctx, user, "ExponentPushToken[readyaaaabbbbcc]", "android", "") //nolint:errcheck

	send := func(ev upload.ReadyEvent) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if err := OnReleaseProcessed(ctx, tx, ev); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	send(upload.ReadyEvent{UploadID: "upl_a", ReleaseID: "alb_1", ReleaseTitle: "My\nEP", OwnerUserID: user, Tracks: 3})
	send(upload.ReadyEvent{UploadID: "upl_b", ReleaseID: "alb_2", ReleaseTitle: "Solo", OwnerUserID: user, Tracks: 1, Live: true})
	send(upload.ReadyEvent{UploadID: "upl_b", ReleaseID: "alb_2", ReleaseTitle: "Solo", OwnerUserID: user, Tracks: 1, Live: true}) // the same event again

	p := pushes(t, pool, user)
	if len(p) != 2 {
		t.Fatalf("pushes = %v", p)
	}
	if p[0]["type"] != "release" || p[0]["id"] != "alb_1" || p[0]["title"] != "Your audio is ready" || p[0]["body"] != "'My EP': 3 tracks are ready" {
		t.Fatalf("push 1 = %v", p[0])
	}
	if p[1]["id"] != "alb_2" || p[1]["body"] != "'Solo': 1 track is ready" {
		t.Fatalf("push 2 = %v", p[1])
	}
	e := emails(t, pool, user)
	if len(e) != 2 {
		t.Fatalf("emails = %+v", e)
	}
	if e[0].subject != "Your music is ready: My EP" || !strings.Contains(e[0].body, `3 tracks in "My EP" finished processing and are ready. Open Blac Music`) {
		t.Fatalf("email 1 = %+v", e[0])
	}
	if !strings.Contains(e[1].body, `The track in "Solo" finished processing and is now playing for your listeners.`) {
		t.Fatalf("email 2 = %+v", e[1])
	}
}
