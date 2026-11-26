package notify

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/blobtest"
	"github.com/blacmusic/backend/internal/push"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/testutil"
	"github.com/blacmusic/backend/internal/transfer"
)

func TestTransferTexts(t *testing.T) {
	for name, c := range map[string]struct {
		e           transfer.Ended
		title, body string
	}{
		"nine songs": {transfer.Ended{TransferID: "tr_1", Status: "completed", Done: 9}, "Import finished", "Your YouTube import is done: 9 songs are on Blac"},
		"one song":   {transfer.Ended{TransferID: "tr_1", Status: "completed", Done: 1}, "Import finished", "Your YouTube import is done: 1 song is on Blac"},
		"no count":   {transfer.Ended{TransferID: "tr_1", Status: "completed"}, "Import finished", "Your YouTube import is done"},
		"two failed": {transfer.Ended{TransferID: "tr_1", Status: "failed", Reason: "items_failed", Done: 5, Failed: 2}, "Import needs attention", "Your import needs attention: 2 songs failed"},
		"one failed": {transfer.Ended{TransferID: "tr_1", Status: "failed", Reason: "items_failed", Failed: 1}, "Import needs attention", "Your import needs attention: 1 song failed"},
		"gave up":    {transfer.Ended{TransferID: "tr_1", Status: "failed", Reason: "attempts_exhausted"}, "Import needs attention", "Your import needs attention: it could not be finished. Open Blac to try again"},
	} {
		m, key := TransferEnded(c.e)
		if m.Title != c.title || m.Body != c.body || m.Type != "transfer" || m.ID != "tr_1" || key != "transfer:tr_1:"+c.e.Status {
			t.Errorf("%s: %+v key %q", name, m, key)
		}
	}
	m, key := ReleaseLive("alb_7", "Mukene")
	if m.Body != "'Mukene' is live" || m.Type != "release" || m.ID != "alb_7" || key != "release-live:alb_7" {
		t.Fatalf("release = %+v %q", m, key)
	}
}

func TestScheduledReleaseGoingLiveQueuesAMessage(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SerializeOutbox(t, pool)
	ctx := context.Background()
	store := release.NewStore(pool, blobtest.New(), "http://api.test")
	store.OnLive = OnReleaseLive
	ps := push.NewStore(pool)

	user := testutil.User(t, pool, "user", true)
	artist := testutil.Artist(t, pool, user, testutil.Unique("art"), 1<<30)
	ps.Register(ctx, user, "ExponentPushToken[notifyaaaabbbb]", "ios", "") //nolint:errcheck
	schedule := func(title string) string {
		rel := testutil.Release(t, pool, artist, title, "single", "scheduled")
		testutil.ReadyTrack(t, pool, user, artist, rel, "t", 1, "draft")
		pool.Exec(ctx, `UPDATE albums SET publish_at = now() - interval '1 minute' WHERE id = $1`, rel) //nolint:errcheck
		return rel
	}
	messages := func() []map[string]string {
		rows, err := pool.Query(ctx, `SELECT title, body, data->>'type', data->>'id' FROM push_messages WHERE user_id = $1 ORDER BY id`, user)
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

	rel := schedule("Blessed")
	if _, err := store.PublishDue(ctx); err != nil {
		t.Fatal(err)
	}
	got := messages()
	if len(got) != 1 || got[0]["body"] != "'Blessed' is live" || got[0]["type"] != "release" || got[0]["id"] != rel || got[0]["title"] != "Your release is live" {
		t.Fatalf("messages = %v", got)
	}
	var vis string
	pool.QueryRow(ctx, `SELECT visibility FROM albums WHERE id = $1`, rel).Scan(&vis) //nolint:errcheck
	if vis != "public" {
		t.Fatalf("release is %s", vis)
	}
	// running it again does nothing more
	store.PublishDue(ctx) //nolint:errcheck
	if len(messages()) != 1 {
		t.Fatal("a second run queued another message")
	}

	// switched off: the release still goes live, silently
	off := false
	ps.SetSettings(ctx, user, push.SettingsPatch{Releases: &off}) //nolint:errcheck
	rel2 := schedule("Silent")
	store.PublishDue(ctx)                                                              //nolint:errcheck
	pool.QueryRow(ctx, `SELECT visibility FROM albums WHERE id = $1`, rel2).Scan(&vis) //nolint:errcheck
	if vis != "public" || len(messages()) != 1 {
		t.Fatalf("switched off: release %s, %d messages", vis, len(messages()))
	}

	// a broken notification hook never keeps a release from going live
	store.OnLive = func(ctx context.Context, tx pgx.Tx, id, title, owner string) error {
		tx.Exec(ctx, `SELECT * FROM table_that_does_not_exist`) //nolint:errcheck
		return errors.New("boom")
	}
	rel3 := schedule("Still live")
	if n, err := store.PublishDue(ctx); err != nil || n < 1 {
		t.Fatalf("PublishDue with a failing hook = %d %v", n, err)
	}
	pool.QueryRow(ctx, `SELECT visibility FROM albums WHERE id = $1`, rel3).Scan(&vis) //nolint:errcheck
	if vis != "public" {
		t.Fatalf("release is %s after a failing hook", vis)
	}
}
