package admin

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blacmusic/backend/internal/genre"
	"github.com/blacmusic/backend/internal/testutil"
)

func TestTaxonomyManagement(t *testing.T) {
	w := newWorld(t)
	pool := w.h.pool
	name := "Zz" + strings.ReplaceAll(testutil.Unique("g"), "g", "")
	if len(name) > 14 {
		name = "Zz" + name[len(name)-10:]
	}
	text := func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	var made [][2]string
	t.Cleanup(func() { // the terms are shared by every test: leave them as they were
		for i := len(made) - 1; i >= 0; i-- {
			w.exec("DELETE FROM "+made[i][0]+" WHERE id = $1", made[i][1])
		}
		if err := genre.Reload(context.Background(), pool); err != nil {
			t.Fatal(err)
		}
	})
	create := func(kind string, body map[string]any) (int, map[string]any) {
		code, b := call(w.r, "POST", "/admin/taxonomy/"+kind, w.adminT, body)
		if code == 201 {
			made = append(made, [2]string{termKinds[kind].table, b["id"].(string)})
		}
		return code, b
	}
	patch := func(kind, id string, body map[string]any) (int, map[string]any) {
		return call(w.r, "PATCH", "/admin/taxonomy/"+kind+"/"+id, w.adminT, body)
	}
	offered := func() string {
		rec := httptest.NewRecorder()
		genre.List(rec, nil)
		return rec.Body.String()
	}

	code, g := create("genres", map[string]any{"label": "  " + name + " Sound "})
	if code != 201 || g["label"] != name+" Sound" || g["active"] != true || g["parentId"] != nil {
		t.Fatalf("create = %d %s", code, text(g))
	}
	id := g["id"].(string)
	ownSlug := w.text(`SELECT slug FROM tax_genres WHERE id = $1`, id)
	list := offered()
	if !strings.Contains(list, `{"id":"`+ownSlug+`","name":"`+name+` Sound"}`) || !strings.HasSuffix(strings.TrimSpace(list), `{"id":"other","name":"Other"}]}`) {
		t.Fatalf("offered = %s", list)
	}
	if got, err := genre.Normalize(strings.ToLower(name) + " sound"); err != nil || got != name+" Sound" {
		t.Fatalf("a new genre is accepted: %q %v", got, err)
	}
	if code, b := create("genres", map[string]any{"label": strings.ToUpper(name) + " SOUND"}); code != 409 || errCode(b) != "duplicate_label" {
		t.Fatalf("duplicate = %d %v", code, b)
	}

	// renaming reaches what stores the name, and the old name still works
	art := w.catalogArtist("Taxonomy Artist")
	rel := testutil.Release(t, pool, art, "R", "album", "public")
	song, _ := testutil.ReadyTrack(t, pool, w.admin, art, rel, "T", 1, "public")
	w.exec(`UPDATE tracks SET genre = $2 WHERE id = $1`, song, name+" Sound")
	w.exec(`UPDATE albums SET genre = $2 WHERE id = $1`, rel, name+" Sound")
	code, g = patch("genres", id, map[string]any{"label": name + " Music"})
	if code != 200 || g["id"] != id || w.text(`SELECT genre FROM tracks WHERE id = $1`, song) != name+" Music" || w.text(`SELECT genre FROM albums WHERE id = $1`, rel) != name+" Music" {
		t.Fatalf("rename = %d %s", code, text(g))
	}
	if got := w.text(`SELECT string_agg(genre_id, ',') FROM track_genres WHERE track_id = $1 AND is_primary`, song); got != id {
		t.Fatalf("link after rename = %s", got)
	}
	if got, err := genre.Normalize(name + " Sound"); err != nil || got != name+" Music" || w.text(`SELECT slug FROM tax_genres WHERE id = $1`, id) != ownSlug {
		t.Fatalf("old name = %q %v", got, err)
	}
	_, tax := call(w.r, "GET", "/admin/taxonomy?usage=true", w.adminT, nil)
	if !strings.Contains(text(tax["genres"]), `"id":"`+id+`","label":"`+name+` Music","parentId":null,"usage":1`) {
		t.Fatalf("usage = %s", text(tax["genres"]))
	}
	if _, plain := call(w.r, "GET", "/admin/taxonomy", w.adminT, nil); strings.Contains(text(plain), `"usage"`) {
		t.Fatal("usage only when asked for")
	}

	// switched off: not offered or newly accepted, but what has it keeps it and can still be edited
	if code, b := patch("genres", id, map[string]any{"active": false}); code != 200 || b["active"] != false {
		t.Fatalf("off = %d %v", code, b)
	}
	if _, err := genre.Normalize(name + " Music"); err == nil || strings.Contains(offered(), name) {
		t.Fatal("a switched-off genre must not be offered or accepted")
	}
	if _, found := genre.Lookup(name + " Music"); !found {
		t.Fatal("browsing by it must still work")
	}
	if code, b := call(w.r, "PATCH", "/admin/releases/"+rel, w.adminT, map[string]any{"genre": name + " Music", "title": "Edited"}); code != 200 {
		t.Fatalf("editing with the genre it has = %d %v", code, b)
	}
	if w.text(`SELECT genre FROM tracks WHERE id = $1`, song) != name+" Music" {
		t.Fatal("songs keep it")
	}

	// one level of subgenres
	_, child := create("genres", map[string]any{"label": name + " Child", "parentId": id})
	if child["parentId"] != id {
		t.Fatalf("child = %v", child)
	}
	childID := child["id"].(string)
	for what, got := range map[string]func() (int, map[string]any){
		"grandchild": func() (int, map[string]any) {
			return create("genres", map[string]any{"label": name + " G", "parentId": childID})
		},
		"itself":              func() (int, map[string]any) { return patch("genres", id, map[string]any{"parentId": id}) },
		"parent with a child": func() (int, map[string]any) { return patch("genres", id, map[string]any{"parentId": "pop"}) },
	} {
		if code, b := got(); code != 422 || errCode(b) != "invalid_parent" {
			t.Fatalf("%s = %d %v", what, code, b)
		}
	}
	if code, b := patch("genres", childID, map[string]any{"parentId": nil}); code != 200 || b["parentId"] != nil {
		t.Fatalf("clear parent = %d %v", code, b)
	}

	code, lang := create("languages", map[string]any{"label": name + " tongue", "groupId": "luo"})
	if code != 201 || lang["groupId"] != "luo" {
		t.Fatalf("language = %d %v", code, lang)
	}
	for what, body := range map[string]map[string]any{"unknown group": {"groupId": "nowhere"}, "no such field": {"parentId": "pop"}, "empty": {"label": " "}} {
		if code, b := patch("languages", lang["id"].(string), body); code != 422 || errCode(b) != "validation_error" {
			t.Fatalf("%s = %d %v", what, code, b)
		}
	}
	if code, b := create("language-groups", map[string]any{"label": "english"}); code != 201 && code != 409 {
		t.Fatalf("group = %d %v", code, b)
	}
	if code, b := create("cultures", map[string]any{"label": "ACHOLI"}); code != 409 {
		t.Fatalf("culture duplicate = %d %v", code, b)
	}
	if code, _ := patch("regions", "mars", map[string]any{"label": "x"}); code != 404 {
		t.Fatalf("unknown = %d", code)
	}
	if code, _ := create("mood", map[string]any{"label": "x"}); code != 404 {
		t.Fatalf("kind = %d", code)
	}
	if w.audit("taxonomy.create", id) != 1 || w.audit("taxonomy.update", id) != 2 {
		t.Fatal("changes must be recorded")
	}
}
