package admin

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/blacmusic/backend/internal/testutil"
)

func TestPreferenceGraph(t *testing.T) {
	w := newWorld(t)
	pool := w.h.pool
	played, picked, bad := w.catalogArtist("Graph Played"), w.catalogArtist("Graph Picked"), w.catalogArtist("Graph Banned")
	song := func(artist, genre string) string {
		rel := testutil.Release(t, pool, artist, "R", "single", "public")
		id, _ := testutil.ReadyTrack(t, pool, w.admin, artist, rel, "T", 1, "public")
		w.exec(`UPDATE tracks SET genre = $2 WHERE id = $1`, id, genre)
		return id
	}
	a, b := song(played, "Gospel"), song(played, "Gospel")
	song(picked, "Reggae")
	banned := song(bad, "Gospel")
	w.exec(`UPDATE artists SET banned_at = now() WHERE id = $1`, bad)
	user := testutil.User(t, pool, "user", true)
	w.exec(`INSERT INTO listener_preferences (user_id, preferred_languages, understood_languages, regions, genres, artist_ids, discovery_mode, completed_at)
		VALUES ($1, '{luganda}', '{english,made_up}', '{west_nile}', '{reggae}', ARRAY[$2, $3], 'familiar', now())`, user, picked, bad)
	w.exec(`INSERT INTO plays (user_id, track_id, artist_id, listened_sec) VALUES ($1, $2, $3, 40), ($1, $2, $3, 40), ($1, $4, $5, 40)`, user, a, played, banned, bad)
	t.Cleanup(func() { w.exec(`DELETE FROM plays WHERE user_id = $1`, user) })
	for i, e := range []string{"like", "unlike", "unlike", "playlist_add"} {
		w.exec(`INSERT INTO listening_events (user_id, event_id, type, at, track_id, session_id) VALUES ($1, $2, $3, now(), $4, 'secret-session')`, user, strconv.Itoa(i), e, a)
	}
	w.exec(`INSERT INTO listening_events (user_id, event_id, type, at, track_id, outcome, replay) VALUES ($1, 'l', 'listen', now(), $2, 'skipped', true),
		($1, 'old', 'listen', now() - interval '100 days', $2, 'completed', false)`, user, b)
	w.exec(`INSERT INTO library_tracks (user_id, track_id) VALUES ($1, $2)`, user, a)

	code, got := call(w.r, "GET", "/admin/users/"+user+"/preference-graph", w.adminT, nil)
	if code != 200 {
		t.Fatalf("graph = %d %v", code, got)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "secret-session") || strings.Contains(string(raw), bad) {
		t.Fatalf("no sessions and no banned artists: %s", raw)
	}
	nodes := map[string]string{}
	for _, n := range got["nodes"].([]any) {
		x, _ := json.Marshal(n)
		nodes[n.(map[string]any)["id"].(string)] = string(x)
	}
	none := `"activity":{"finished":0,"likes":0,"playlistAdds":0,"plays":0,"replays":0,"savedSongs":0,"skipped":0}`
	for id, want := range map[string]string{
		"language:luganda": `"chosen":true`, "language:english": `"chosen":false`, "language:made_up": `"label":"made_up"`,
		"region:west_nile": none, "genre:reggae": `"chosen":true`, "artist:" + picked: `"followed":false`,
		"artist:" + played: `"activity":{"finished":0,"likes":0,"playlistAdds":1,"plays":2,"replays":1,"savedSongs":1,"skipped":1}`,
	} {
		if !strings.Contains(nodes[id], want) {
			t.Fatalf("%s = %s, want %s", id, nodes[id], want)
		}
	}
	// eight nodes: nothing about the person is made up from what they played
	if len(nodes) != 8 || nodes["genre:gospel"] == "" {
		t.Fatalf("nodes = %v", nodes)
	}
	links, _ := json.Marshal(got["links"])
	for _, want := range []string{
		`{"kind":"artist_genre","source":"artist:` + played + `","target":"genre:gospel","weight":2}`,
		`{"kind":"artist_genre","source":"artist:` + picked + `","target":"genre:reggae","weight":1}`,
	} {
		if !strings.Contains(string(links), want) {
			t.Fatalf("links = %s", links)
		}
	}
	if code, _ := call(w.r, "GET", "/admin/users/00000000-0000-0000-0000-000000000000/preference-graph", w.adminT, nil); code != 404 {
		t.Fatalf("unknown user = %d", code)
	}
	if code, _ := call(w.r, "GET", "/admin/users/"+user+"/preference-graph", testutil.Token(t, user), nil); code != 403 {
		t.Fatalf("a user = %d", code)
	}
}
