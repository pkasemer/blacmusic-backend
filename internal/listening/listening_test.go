package listening

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/testutil"
)

func TestListeningEventsAndYourSound(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	h := NewHandler(pool)
	r := chi.NewRouter()
	r.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
	r.Post("/events", h.Events)
	r.Get("/your-sound", h.YourSound)

	owner := testutil.User(t, pool, "user", true)
	artist := testutil.Artist(t, pool, owner, testutil.Unique("Loved"), testutil.GiB)
	rel := testutil.Release(t, pool, artist, "R", "album", "public")
	var songs []string
	for range 10 {
		id, _ := testutil.ReadyTrack(t, pool, owner, artist, rel, "S", 10, "public")
		songs = append(songs, id)
	}
	user := testutil.User(t, pool, "user", true)
	token := testutil.Token(t, user)
	call := func(method, path string, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	one := func(q string, args ...any) (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	listen := func(track, outcome, extra string) string {
		n++
		heard := "90"
		if outcome == "skipped" {
			heard = "20" // 90 or more is a finished song, whatever it is called
		}
		return `{"id":"e` + strconv.Itoa(n) + `","type":"listen","at":"` + time.Now().Add(-time.Hour).Format(time.RFC3339) +
			`","sessionId":"s1","trackId":"` + track + `","capture":2,"listenedSec":9,"durationSec":10,"completion":` + heard + `,"outcome":"` + outcome + `","replay":false` + extra + `}`
	}
	send := func(events ...string) int {
		var b bytes.Buffer
		b.WriteString(`{"events":[`)
		for i, e := range events {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(e)
		}
		b.WriteString(`]}`)
		code, _ := call("POST", "/events", b.String())
		return code
	}

	// a bad event is left out, the rest are kept, and sending again changes nothing
	good := listen(songs[0], "completed", `,"source":"trending","new":"field"`)
	batch := []string{good, listen("trk_gone", "completed", ""), listen(songs[0], "completed", `,"source":"moon"`), `{"id":5}`,
		`{"id":"start","type":"session_start","at":"` + time.Now().Format(time.RFC3339) + `","sessionId":"s1"}`,
		listen(songs[1], "skipped", ""), listen(songs[1], "skipped", "")}
	for range 2 {
		if code := send(batch...); code != 204 {
			t.Fatalf("events = %d", code)
		}
	}
	if got := one(`SELECT count(*) FROM listening_events WHERE user_id = $1`, user); got != 4 {
		t.Fatalf("stored %d events, want 4", got)
	}
	if got := one(`SELECT count(*) FROM plays WHERE user_id = $1`, user); got != 0 {
		t.Fatal("events are not plays")
	}
	// autoplay's songs are stored as such, with the song they were chosen from and how the listen ended
	if code := send(`{"id":"auto","type":"listen","at":"` + time.Now().Format(time.RFC3339) + `","sessionId":"s1","trackId":"` + songs[0] +
		`","listenedSec":10,"durationSec":10,"completion":100,"outcome":"completed","source":"search","autoplay":true,"autoplaySeedId":"` + songs[1] +
		`","endReason":"ended","positionSec":10,"capture":2}`); code != 204 {
		t.Fatalf("autoplay = %d", code)
	}
	if one(`SELECT count(*) FROM listening_events WHERE user_id = $1 AND source = 'autoplay' AND seed_track_id = $2
		AND end_reason = 'ended' AND end_position_sec = 10 AND capture = 2`, user, songs[1]) != 1 {
		t.Fatal("autoplay was not stored")
	}
	if code, _ := call("POST", "/events", `[1]`); code != 400 {
		t.Fatalf("malformed = %d", code)
	}

	exec(`INSERT INTO library_artists (user_id, artist_id) VALUES ($1, $2)`, user, artist) // they follow the artist

	// the playlist the app made is taken over, with its songs left alone
	var made string
	if err := pool.QueryRow(ctx, `INSERT INTO playlists (owner_id, name, description) VALUES ($1, $2, $3) RETURNING id`, user, soundName, soundDescription).Scan(&made); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO playlist_items (playlist_id, track_id, position, added_by) VALUES ($1, $2, 0, $3)`, made, songs[1], user) // skipped, but theirs
	var id string
	for range 2 {
		code, b := call("GET", "/your-sound", "")
		if code != 200 || b["playlistId"] != made {
			t.Fatalf("your sound = %d %v, want %s", code, b, made)
		}
		id = made
	}
	added := func() int { return one(`SELECT count(*) FROM playlist_items WHERE playlist_id = $1 AND by_server`, id) }
	// songs 0 was just heard, song 1 is in it: at most 4 new of one artist at a time
	if added() != 4 || one(`SELECT count(*) FROM playlist_items WHERE playlist_id = $1 AND by_server AND track_id IN ($2, $3)`, id, songs[0], songs[1]) != 0 {
		t.Fatalf("added = %d", added())
	}
	if one(`SELECT count(*) FROM playlist_items WHERE playlist_id = $1 AND position NOT BETWEEN 0 AND 4`, id) != 0 {
		t.Fatal("positions must be 0..n-1")
	}

	// one the listener takes out stays out; one they keep skipping is dropped; their own song stays
	var removed, skipped string
	if err := pool.QueryRow(ctx, `SELECT min(track_id), max(track_id) FROM playlist_items WHERE playlist_id = $1 AND by_server`, id).Scan(&removed, &skipped); err != nil {
		t.Fatal(err)
	}
	exec(`DELETE FROM playlist_items WHERE playlist_id = $1 AND track_id = $2`, id, removed)
	if code := send(listen(skipped, "skipped", ""), listen(skipped, "skipped", "")); code != 204 {
		t.Fatalf("skips = %d", code)
	}
	exec(`UPDATE your_sound SET refreshed_at = now() - interval '2 days' WHERE user_id = $1`, user)
	if done, err := RefreshDue(ctx, pool, 1000); err != nil || done < 1 {
		t.Fatalf("refresh = %d %v", done, err)
	}
	if one(`SELECT count(*) FROM playlist_items WHERE playlist_id = $1 AND track_id IN ($2, $3)`, id, removed, skipped) != 0 {
		t.Fatal("a removed or skipped song came back or stayed")
	}
	if one(`SELECT count(*) FROM playlist_items WHERE playlist_id = $1 AND track_id = $2 AND NOT by_server`, id, songs[1]) != 1 || added() <= 2 {
		t.Fatalf("their own song stays and new ones arrive: %d", added())
	}

	// deleting it gets a new, private one, and never a second
	exec(`DELETE FROM playlists WHERE id = $1`, id)
	_, a := call("GET", "/your-sound", "")
	_, b := call("GET", "/your-sound", "")
	if a["playlistId"] == made || a["playlistId"] != b["playlistId"] ||
		one(`SELECT count(*) FROM playlists WHERE owner_id = $1 AND name = $2 AND NOT is_public`, user, soundName) != 1 {
		t.Fatalf("new playlist = %v %v", a, b)
	}

	exec(`DELETE FROM users WHERE id = $1`, user)
	if one(`SELECT (SELECT count(*) FROM listening_events WHERE user_id = $1) + (SELECT count(*) FROM your_sound WHERE user_id = $1)
		+ (SELECT count(*) FROM your_sound_offers WHERE user_id = $1)`, user) != 0 {
		t.Fatal("everything goes with the account")
	}
}
