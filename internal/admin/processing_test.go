package admin

import (
	"context"
	"testing"

	"github.com/blacmusic/backend/internal/testutil"
)

// queueSong puts an upload of a catalog artist on the processing list; set says what state it is in.
func (w *world) queueSong(artistID, releaseID, set string) (id, key string) {
	w.t.Helper()
	id = testutil.Unique("upl_proc")
	key = "originals/" + id
	w.exec(`INSERT INTO uploads (id, user_id, artist_id, release_id, title, artist_name, album_title, genre, filename, size_bytes,
	        original_key, rights_terms_version, rights_accepted_at, by_team, quota_exempt)
	        VALUES ($1, $2, $3, $4, 'Stuck Song', 'A', 'R', '', 'x.mp3', 5, $5, 'test', now(), true, true)`, id, w.admin, artistID, releaseID, key)
	w.exec(`UPDATE uploads SET `+set+` WHERE id = $1`, id)
	w.blobs.Put(key, []byte("audio"))
	return id, key
}

// listed finds one upload on the processing list.
func (w *world) listed(id string) map[string]any {
	w.t.Helper()
	code, b := call(w.r, "GET", "/admin/processing", w.adminT, nil)
	if code != 200 {
		w.t.Fatalf("processing = %d %v", code, b)
	}
	for _, u := range b["uploads"].([]any) {
		if m := u.(map[string]any); m["uploadId"] == id {
			return m
		}
	}
	return nil
}

func TestStuckSongsCanBeRetried(t *testing.T) {
	w := newWorld(t)
	art := w.catalogArtist("Stuck Artist")
	rel := testutil.Release(t, w.h.pool, art, "R", "single", "draft")
	// leave nothing queued behind for the tests that claim jobs
	t.Cleanup(func() { w.exec(`DELETE FROM uploads WHERE artist_id = $1`, art) })
	retry := func(id string, body any) (int, map[string]any) {
		return call(w.r, "POST", "/admin/processing/"+id+"/retry", w.adminT, body)
	}
	const silent = `status = 'processing', attempts = 3, claimed_at = now() - interval '20 minutes', locked_at = now() - interval '4 minutes'`
	const busy = `status = 'processing', attempts = 1, claimed_at = now() - interval '20 minutes', locked_at = now() - interval '20 seconds'`

	stuck, _ := w.queueSong(art, rel, silent)
	working, _ := w.queueSong(art, rel, busy)
	queued, _ := w.queueSong(art, rel, `status = 'queued'`)
	ready, _ := w.queueSong(art, rel, `status = 'ready'`)
	failed, _ := w.queueSong(art, rel, `status = 'failed', error = 'processing did not finish, please upload again', attempts = 3`)
	gone, goneKey := w.queueSong(art, rel, `status = 'failed', error = 'not audio'`)
	if err := w.blobs.Remove(context.Background(), goneKey); err != nil {
		t.Fatal(err)
	}

	user := testutil.Token(t, testutil.User(t, w.h.pool, "user", true))
	for _, req := range [][2]string{{"GET", "/admin/processing"}, {"POST", "/admin/processing/" + stuck + "/retry"}} {
		if code, _ := call(w.r, req[0], req[1], user, nil); code != 403 {
			t.Fatalf("%v by a user = %d, want 403", req, code)
		}
	}

	for id, want := range map[string]struct {
		state           string
		canRetry, force bool
	}{stuck: {"stuck", true, false}, working: {"working", false, true}, queued: {"waiting", false, false}, failed: {"failed", true, false}, gone: {"failed", false, false}} {
		got := w.listed(id)
		if got == nil || got["state"] != want.state || got["canRetry"] != want.canRetry || got["canForce"] != want.force || got["artistId"] != art || got["releaseId"] != rel {
			t.Fatalf("listed %s = %v, want %+v", id, got, want)
		}
	}
	if w.listed(ready) != nil {
		t.Fatal("a finished song is not on the list")
	}
	code, b := call(w.r, "GET", "/admin/processing", w.adminT, nil)
	if code != 200 || b["counts"].(map[string]any)["stuck"].(float64) < 1 || b["stuckAfterSeconds"] != float64(180) ||
		b["uploads"].([]any)[0].(map[string]any)["state"] != "stuck" {
		t.Fatalf("stuck songs must be counted and come first: %v", b)
	}
	code, b = call(w.r, "GET", "/admin/system", w.adminT, nil)
	alerted := false
	for _, a := range b["alerts"].([]any) {
		alerted = alerted || a.(map[string]any)["code"] == "upload_stuck"
	}
	if code != 200 || !alerted || b["work"].(map[string]any)["uploads"].(map[string]any)["stuck"].(float64) < 1 {
		t.Fatalf("the system page must warn about a stuck song: %v %v", b["alerts"], b["work"])
	}

	for id, want := range map[string]string{working: "still_processing", queued: "already_queued", ready: "not_stuck", gone: "file_gone"} {
		if code, b := retry(id, nil); code != 409 || errCode(b) != want {
			t.Fatalf("retry %s = %d %v, want 409 %s", id, code, b, want)
		}
	}
	if code, _ := retry("upl_nothing", nil); code != 404 {
		t.Fatalf("retry of an unknown upload = %d", code)
	}

	for id, body := range map[string]any{stuck: nil, failed: map[string]any{}, working: map[string]any{"force": true}} {
		code, b := retry(id, body)
		if code != 200 || b["state"] != "waiting" || b["attempts"] != float64(0) || b["error"] != "" || b["startedAt"] != nil || b["canRetry"] != false {
			t.Fatalf("retry %s = %d %v", id, code, b)
		}
		if w.audit("upload.retry", id) != 1 {
			t.Fatalf("retry of %s was not recorded", id)
		}
	}
	if code, b := retry(stuck, nil); code != 409 || errCode(b) != "already_queued" {
		t.Fatalf("a second click = %d %v", code, b)
	}
}
