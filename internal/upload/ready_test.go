package upload

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/testutil"
)

// finishUpload reserves an upload on a release and plays the worker finishing it.
func (e *env) finishUpload(userID, releaseID, title string) error {
	e.t.Helper()
	ctx := context.Background()
	u, err := e.store.Create(ctx, NewUpload{UserID: userID, ReleaseID: releaseID, Title: title, Filename: "a.flac", SizeBytes: 100, TermsVersion: "2024-01"})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE uploads SET status = 'processing' WHERE id = $1`, u.ID); err != nil {
		e.t.Fatal(err)
	}
	u, err = e.store.Get(ctx, u.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.store.Finish(ctx, u, Result{DurationMs: 1000, Renditions: []Rendition{{Quality: "medium", MediaKey: "tracks/" + u.TrackID + "/medium.m4a", BitrateKbps: 128, SizeBytes: 10}}})
}

func TestReadyHookFiresWhenTheLastFileOfAReleaseIsDone(t *testing.T) {
	e := setup(t)
	_, userID, artistID := e.artistUser(testutil.GiB)
	var events []ReadyEvent
	e.store.OnReady = func(_ context.Context, _ pgx.Tx, ev ReadyEvent) error {
		events = append(events, ev)
		return nil
	}
	ctx := context.Background()
	rel := testutil.Release(t, e.pool, artistID, "Two Songs", "album", "draft")

	// both files are reserved before either finishes: only the last one to finish reports
	first, err := e.store.Create(ctx, NewUpload{UserID: userID, ReleaseID: rel, Title: "One", Filename: "a.flac", SizeBytes: 100, TermsVersion: "x"})
	if err != nil {
		t.Fatal(err)
	}
	e.pool.Exec(ctx, `UPDATE uploads SET status = 'processing' WHERE id = $1`, first.ID) //nolint:errcheck
	first, _ = e.store.Get(ctx, first.ID)
	if err := e.finishUpload(userID, rel, "Two"); err != nil { // reserved after "One", finished before it
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("an upload is still unfinished, yet %v", events)
	}
	if err := e.store.Finish(ctx, first, Result{DurationMs: 1000, Renditions: []Rendition{{Quality: "medium", MediaKey: "tracks/" + first.TrackID + "/medium.m4a", BitrateKbps: 128, SizeBytes: 10}}}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %v, want one", events)
	}
	ev := events[0]
	if ev.ReleaseID != rel || ev.ReleaseTitle != "Two Songs" || ev.OwnerUserID != userID || ev.Tracks != 2 || ev.Live || ev.UploadID != first.ID {
		t.Fatalf("event = %+v", ev)
	}

	// a later single upload reports again, and says when the release is already live
	e.pool.Exec(ctx, `UPDATE albums SET visibility = 'public' WHERE id = $1`, rel) //nolint:errcheck
	if err := e.finishUpload(userID, rel, "Three"); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || !events[1].Live || events[1].Tracks != 3 {
		t.Fatalf("events = %+v", events)
	}
}

func TestAFailingReadyHookNeverKeepsATrackFromFinishing(t *testing.T) {
	e := setup(t)
	_, userID, artistID := e.artistUser(testutil.GiB)
	e.store.OnReady = func(ctx context.Context, tx pgx.Tx, _ ReadyEvent) error {
		tx.Exec(ctx, `SELECT * FROM table_that_does_not_exist`) //nolint:errcheck
		return errors.New("boom")
	}
	rel := testutil.Release(t, e.pool, artistID, "Fragile", "single", "draft")
	if err := e.finishUpload(userID, rel, "Still here"); err != nil {
		t.Fatalf("finish with a failing hook = %v", err)
	}
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM tracks WHERE album_id = $1`, rel).Scan(&n) //nolint:errcheck
	if n != 1 {
		t.Fatalf("tracks = %d, want 1", n)
	}
}
