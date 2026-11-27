package admin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/blacmusic/backend/internal/testutil"
)

func TestAdminLyrics(t *testing.T) {
	w := newWorld(t)
	art := w.catalogArtist("Lyric Artist")
	rel := testutil.Release(t, w.h.pool, art, "R", "single", "draft")
	song, _ := testutil.ReadyTrack(t, w.h.pool, w.admin, art, rel, "Sung", 1, "draft")
	w.exec(`UPDATE tracks SET duration_ms = 60000 WHERE id = $1`, song)
	path := "/admin/tracks/" + song + "/lyrics"
	text := func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}

	if code, b := call(w.r, "GET", path, w.adminT, nil); code != 200 || b["lyrics"] != nil || b["durationMs"] != float64(60000) {
		t.Fatalf("none = %d %s", code, text(b))
	}
	code, b := call(w.r, "PUT", path, w.adminT, map[string]any{"lrc": "[00:01.00]Nkwagala\nI love you\n[00:05.00]Oh"})
	if code != 200 || b["untranslatedLines"] != float64(1) || !strings.Contains(text(b), `"translation":"I love you"`) || !strings.Contains(text(b), `"text":"Nkwagala\nOh"`) {
		t.Fatalf("save = %d %s", code, text(b))
	}
	code, b = call(w.r, "PUT", path, w.adminT, map[string]any{"lrc": "[02:00.00]Too late\n\nlost"})
	if code != 422 || errCode(b) != "validation_error" || !strings.Contains(text(b), `{"line":3,`) || !strings.Contains(text(b), "after the song ends (1:00)") {
		t.Fatalf("invalid = %d %s", code, text(b))
	}
	if _, b := call(w.r, "GET", path, w.adminT, nil); !strings.Contains(text(b), "Nkwagala") {
		t.Fatal("a refused save changes nothing")
	}
	// lyrics saved before the typed text was kept can still be edited whole
	w.exec(`UPDATE track_lyrics SET lrc = '' WHERE track_id = $1`, song)
	if _, b := call(w.r, "GET", path, w.adminT, nil); b["lyrics"].(map[string]any)["lrc"] != "[00:01.00]Nkwagala\nI love you\n[00:05.00]Oh" {
		t.Fatalf("lrc = %s", text(b))
	}
	if _, b := call(w.r, "GET", "/admin/releases/"+rel, w.adminT, nil); !strings.Contains(text(b["tracks"]), `"hasLyrics":true`) {
		t.Fatalf("release tracks = %s", text(b["tracks"]))
	}
	if code, _ := call(w.r, "PUT", path, testutil.Token(t, testutil.User(t, w.h.pool, "user", true)), map[string]any{"text": "x"}); code != 403 {
		t.Fatalf("a user = %d", code)
	}
	for range 2 {
		if code, _ := call(w.r, "DELETE", path, w.adminT, nil); code != 204 {
			t.Fatalf("delete = %d", code)
		}
	}
	if w.audit("track.lyrics_update", song) != 1 || w.audit("track.lyrics_delete", song) != 1 {
		t.Fatal("changes must be recorded once each")
	}
	if code, _ := call(w.r, "PUT", "/admin/tracks/trk_nothing/lyrics", w.adminT, map[string]any{"text": "x"}); code != 404 {
		t.Fatalf("unknown = %d", code)
	}
}
