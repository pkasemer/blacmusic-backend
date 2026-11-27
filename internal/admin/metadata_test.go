package admin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/blacmusic/backend/internal/testutil"
)

func TestCatalogueMetadata(t *testing.T) {
	w := newWorld(t)
	art := w.catalogArtist("Metadata Artist")
	pool := w.h.pool
	rel := testutil.Release(t, pool, art, "Described", "album", "public")
	first, _ := testutil.ReadyTrack(t, pool, w.admin, art, rel, "One", 100, "public")
	second, _ := testutil.ReadyTrack(t, pool, w.admin, art, rel, "Two", 100, "public")
	w.exec(`UPDATE tracks SET track_number = CASE WHEN id = $1 THEN 1 ELSE 2 END WHERE album_id = $2`, first, rel)
	w.exec(`UPDATE tracks SET genre = 'Gospel' WHERE id = $1`, first)
	w.exec(`INSERT INTO release_credits (release_id, name, role) VALUES ($1, 'Guest', 'featured_artist'), ($1, 'Maker', 'producer')`, rel)

	text := func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	get := func(id string) map[string]any {
		t.Helper()
		code, b := call(w.r, "GET", "/admin/tracks/"+id+"/metadata", w.adminT, nil)
		if code != 200 {
			t.Fatalf("get = %d %v", code, b)
		}
		return b
	}
	put := func(id string, version any, body string) (int, map[string]any) {
		var m map[string]any
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatal(err)
		}
		m["version"] = version
		return call(w.r, "PUT", "/admin/tracks/"+id+"/metadata", w.adminT, m)
	}

	code, tax := call(w.r, "GET", "/admin/taxonomy", w.adminT, nil)
	if code != 200 || !strings.Contains(text(tax["languages"]), `{"active":true,"groupId":"central_eastern_bantu","id":"lussese","label":"Lussese"}`) ||
		!strings.Contains(text(tax["genres"]), `"id":"kadongo_kamu"`) || len(tax["cultures"].([]any)) != 13 || len(tax["attributes"].(map[string]any)["mood"].([]any)) != 9 {
		t.Fatalf("taxonomy = %d %s", code, text(tax))
	}
	user := testutil.Token(t, testutil.User(t, pool, "user", true))
	if code, _ := call(w.r, "GET", "/admin/tracks/"+first+"/metadata", user, nil); code != 403 {
		t.Fatalf("a user = %d", code)
	}

	// the genre a song already had is there, unverified, and does not count as started
	m := get(first)
	if m["status"] != "not_started" || m["languageUnknown"] != false || m["updatedAt"] != nil || m["isrc"] != nil || len(m["languages"].([]any)) != 0 ||
		text(m["featuredArtists"]) != `["Guest"]` || !strings.Contains(text(m["genres"]), `"id":"gospel","primary":true,"source":"imported","verification":"pending"`) &&
		!strings.Contains(text(m["genres"]), `"source":"imported"`) {
		t.Fatalf("before = %s", text(m))
	}
	code, list := call(w.r, "GET", "/admin/releases/"+rel+"/track-metadata", w.adminT, nil)
	if code != 200 || len(list["tracks"].([]any)) != 2 || list["tracks"].([]any)[1].(map[string]any)["trackId"] != second {
		t.Fatalf("release = %d %s", code, text(list))
	}

	full := `{"languages":[{"id":"luganda","role":"primary"},{"id":"english","role":"secondary"}],"cultures":["baganda"],"regions":["central_uganda"],
		"genres":[{"id":"gospel","primary":false},{"id":"kadongo_kamu","primary":true}],"attributes":{"mood":["happy","spiritual"],"energy":["high"],"instrumentation":["guitar"]}}`
	version := m["version"]
	code, saved := put(first, version, full)
	if code != 200 || saved["status"] != "pending_review" || saved["version"] == version || saved["updatedBy"] == nil || len(saved["languages"].([]any)) != 2 {
		t.Fatalf("save = %d %s", code, text(saved))
	}
	if got := w.text(`SELECT genre FROM tracks WHERE id = $1`, first); got != "Kadongo Kamu" {
		t.Fatalf("tracks.genre = %q", got)
	}
	// the imported genre keeps where it came from
	if got := w.text(`SELECT string_agg(genre_id || ':' || source || ':' || is_primary, ',' ORDER BY genre_id) FROM track_genres WHERE track_id = $1`, first); got != "gospel:imported:false,kadongo_kamu:admin:true" {
		t.Fatalf("genres = %s", got)
	}
	if code, got := put(first, version, full); code != 409 || errCode(got) != "stale_version" {
		t.Fatalf("stale = %d %v", code, got)
	}
	if code, got := put(first, saved["version"], full); code != 200 || got["version"] != saved["version"] || w.audit("track.metadata_update", first) != 1 {
		t.Fatalf("saving nothing new = %d %v", code, got["version"])
	}

	// a verified language that changes role goes back to be verified
	w.exec(`UPDATE track_languages SET verification = 'verified', verified_by = $2, verified_at = now() WHERE track_id = $1`, first, w.admin)
	code, swapped := put(first, saved["version"], `{"languages":[{"id":"english","role":"primary"},{"id":"luganda","role":"secondary"}]}`)
	if code != 200 || strings.Contains(text(swapped["languages"]), `"verification":"verified"`) || swapped["languages"].([]any)[0].(map[string]any)["id"] != "english" ||
		len(swapped["cultures"].([]any)) != 0 || w.text(`SELECT genre FROM tracks WHERE id = $1`, first) != "" {
		t.Fatalf("swap = %d %s", code, text(swapped))
	}

	w.exec(`UPDATE tax_cultures SET active = false WHERE id = 'alur'`)
	t.Cleanup(func() { w.exec(`UPDATE tax_cultures SET active = true WHERE id = 'alur'`) })
	for body, want := range map[string]string{
		`{"languages":[{"id":"english","role":"primary"},{"id":"luganda","role":"primary"}]}`:                       "validation_error",
		`{"languageUnknown":true,"languages":[{"id":"english","role":"primary"}]}`:                                  "validation_error",
		`{"genres":[{"id":"gospel","primary":true},{"id":"pop","primary":true}]}`:                                   "validation_error",
		`{"attributes":{"energy":["low","high"]}}`:                                                                  "validation_error",
		`{"cultures":["baganda","baganda"]}`:                                                                        "validation_error",
		`{"languages":[{"id":"klingon","role":"primary"}]}`:                                                         "invalid_taxonomy",
		`{"attributes":{"mood":["high"]}}`:                                                                          "invalid_taxonomy",
		`{"languages":[{"id":"english","role":"primary"},{"id":"luganda","role":"secondary"}],"cultures":["alur"]}`: "invalid_taxonomy",
	} {
		if code, got := put(first, swapped["version"], body); code != 422 || errCode(got) != want {
			t.Fatalf("%s = %d %v, want %s", body, code, got, want)
		}
	}
	if code, _ := put("trk_nothing", "0", `{}`); code != 404 {
		t.Fatalf("unknown song = %d", code)
	}

	// "unknown" is a statement of its own; clearing everything is not started again
	code, unknown := put(second, get(second)["version"], `{"languageUnknown":true}`)
	if code != 200 || unknown["status"] != "pending_review" || unknown["languageUnknown"] != true {
		t.Fatalf("unknown = %d %s", code, text(unknown))
	}
	if code, cleared := put(second, unknown["version"], `{}`); code != 200 || cleared["status"] != "not_started" {
		t.Fatalf("cleared = %d %s", code, text(cleared))
	}

	// a genre changed elsewhere (the song's edit form) moves the main genre and makes an open editor out of date
	open := get(first)["version"]
	w.exec(`UPDATE tracks SET genre = 'Pop' WHERE id = $1`, first)
	after := get(first)
	if after["version"] == open || !strings.Contains(text(after["genres"]), `"id":"pop"`) {
		t.Fatalf("genre sync = %s", text(after))
	}
	w.exec(`DELETE FROM tracks WHERE id = $1`, first) // a song takes its metadata with it
}
