package admin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/blacmusic/backend/internal/testutil"
)

func seedSearch(t *testing.T, f *full, query, kind string, results int, daysAgo int) {
	t.Helper()
	if _, err := f.h.pool.Exec(context.Background(), `INSERT INTO search_queries (at, kind, query, results) VALUES (now() - make_interval(days => $4, mins => 1), $2, $1, $3)`,
		query, kind, results, daysAgo); err != nil {
		t.Fatal(err)
	}
}

func sumPoints(b map[string]any) int {
	n := 0
	for _, p := range b["points"].([]any) {
		n += int(p.(map[string]any)["count"].(float64))
	}
	return n
}

func TestSearchInsights(t *testing.T) {
	f := setupFull(t)
	tag := strings.ToLower(testutil.Unique("ins"))
	_, before := call(f.r, "GET", "/admin/search/overview?range=7d", f.adminT, nil)
	_, tsBefore := call(f.r, "GET", "/admin/stats/timeseries?metric=searches&bucket=day", f.adminT, nil)
	_, tsNoBefore := call(f.r, "GET", "/admin/stats/timeseries?metric=searches_no_results", f.adminT, nil)

	// "wanted": 4 searches this week (2 empty), 1 the week before: rising. "steady": 3 and 3. "gone": none this week.
	for i := 0; i < 2; i++ {
		seedSearch(t, f, tag+" wanted", "tracks", 0, 1)
		seedSearch(t, f, tag+" wanted", "tracks", 5, 2)
	}
	seedSearch(t, f, tag+" wanted", "tracks", 0, 9)
	for i := 0; i < 3; i++ {
		seedSearch(t, f, tag+" steady", "artists", 4, 1)
		seedSearch(t, f, tag+" steady", "artists", 4, 10)
	}
	seedSearch(t, f, tag+" gone", "albums", 0, 20)
	seedSearch(t, f, tag+" playlist only", "playlists", 0, 1)

	// overview: this week gained 4+3+1 = 8 searches, 2+1 = 3 of them empty; the week before gained 1+3 = 4
	_, after := call(f.r, "GET", "/admin/search/overview?range=7d", f.adminT, nil)
	diff := func(path ...string) float64 {
		a, b := after, before
		for _, k := range path[:len(path)-1] {
			a, b = a[k].(map[string]any), b[k].(map[string]any)
		}
		k := path[len(path)-1]
		return a[k].(float64) - b[k].(float64)
	}
	if diff("searches") != 8 || diff("noResultSearches") != 3 || diff("uniqueQueries") != 3 || diff("previous", "searches") != 4 || diff("previous", "noResultSearches") != 1 {
		t.Fatalf("overview before %v after %v", before, after)
	}
	if r, _ := after["noResultRate"].(float64); r < 0 || r > 1 {
		t.Fatalf("noResultRate = %v", after["noResultRate"])
	}

	code, b := call(f.r, "GET", "/admin/search/top?range=7d&q="+tag, f.adminT, nil)
	items, _ := b["items"].([]any)
	if code != 200 || len(items) != 3 || b["total"] != float64(3) {
		t.Fatalf("top = %d %v", code, b)
	}
	first := items[0].(map[string]any)
	if first["query"] != tag+" wanted" || first["count"] != float64(4) || first["noResultCount"] != float64(2) || first["previousCount"] != float64(1) || first["lastAt"] == nil {
		t.Fatalf("top[0] = %v", first)
	}
	// rising: only queries searched 3+ times this week, most growth first (wanted: 4 vs 1; steady: 3 vs 3)
	code, b = call(f.r, "GET", "/admin/search/top?range=7d&sort=rising&q="+tag, f.adminT, nil)
	items, _ = b["items"].([]any)
	if code != 200 || len(items) != 2 || items[0].(map[string]any)["query"] != tag+" wanted" || items[1].(map[string]any)["query"] != tag+" steady" {
		t.Fatalf("rising = %d %v", code, b)
	}
	if _, b = call(f.r, "GET", "/admin/search/top?range=7d&kind=artists&q="+tag, f.adminT, nil); b["total"] != float64(1) {
		t.Fatalf("kind filter = %v", b)
	}
	if _, b = call(f.r, "GET", "/admin/search/top?range=30d&q="+tag+"&limit=1&offset=1", f.adminT, nil); len(b["items"].([]any)) != 1 || b["total"] != float64(5) && b["total"] != float64(4) {
		t.Fatalf("paging keeps the total: %v", b)
	}
	for _, p := range []string{"range=1y", "kind=songs", "sort=hot"} {
		if code, _ = call(f.r, "GET", "/admin/search/top?"+p, f.adminT, nil); code != 400 {
			t.Errorf("%s = %d, want 400", p, code)
		}
	}

	// the content team's wish list
	code, b = call(f.r, "GET", "/admin/search/no-results?range=7d&kind=tracks", f.adminT, nil)
	items, _ = b["items"].([]any)
	var wanted map[string]any
	for _, it := range items {
		if m := it.(map[string]any); m["query"] == tag+" wanted" {
			wanted = m
		}
	}
	if code != 200 || wanted == nil || wanted["count"] != float64(2) || b["total"] == nil {
		t.Fatalf("no-results = %d %v", code, b)
	}
	if _, b = call(f.r, "GET", "/admin/search/no-results?range=7d&kind=albums", f.adminT, nil); strings.Contains(string(mustJSON(b)), tag+" gone") {
		t.Fatalf("a search from 20 days ago is outside 7d: %v", b)
	}

	code, b = call(f.r, "GET", "/admin/search/kinds?range=30d", f.adminT, nil)
	kinds, _ := b["items"].([]any)
	if code != 200 || len(kinds) != 4 {
		t.Fatalf("kinds = %d %v", code, b)
	}

	// the daily series
	_, tsAfter := call(f.r, "GET", "/admin/stats/timeseries?metric=searches&bucket=day", f.adminT, nil)
	_, tsNoAfter := call(f.r, "GET", "/admin/stats/timeseries?metric=searches_no_results", f.adminT, nil)
	if sumPoints(tsAfter)-sumPoints(tsBefore) != 13 || sumPoints(tsNoAfter)-sumPoints(tsNoBefore) != 5 {
		t.Fatalf("timeseries gained %d searches and %d empty ones, want 13 and 5 (the default window is 30 days)", sumPoints(tsAfter)-sumPoints(tsBefore), sumPoints(tsNoAfter)-sumPoints(tsNoBefore))
	}
	// only administrators read any of it
	user := testutil.Token(t, testutil.User(t, f.h.pool, "user", true))
	if code, _ = call(f.r, "GET", "/admin/search/overview", user, nil); code != 403 {
		t.Fatalf("a normal user = %d, want 403", code)
	}
}

func mustJSON(b map[string]any) []byte {
	out, _ := json.Marshal(b)
	return out
}

func TestHomeSectionChangesAreAudited(t *testing.T) {
	f := setupFull(t)
	id := strings.ToLower(testutil.Unique("sec"))
	code, b := call(f.r, "POST", "/admin/playlist-groups", f.adminT, map[string]any{"id": id, "title": "Evening " + id})
	if code != 201 {
		t.Fatalf("create = %d %v", code, b)
	}
	if code, _ = call(f.r, "PATCH", "/admin/playlist-groups/"+id, f.adminT, map[string]any{"title": "Late " + id, "isActive": false}); code != 200 {
		t.Fatalf("update = %d", code)
	}
	if code, _ = call(f.r, "PUT", "/admin/playlist-groups/"+id+"/playlists", f.adminT, map[string]any{"playlistIds": []string{}}); code != 200 {
		t.Fatalf("set playlists = %d", code)
	}
	if code, _ = call(f.r, "DELETE", "/admin/playlist-groups/"+id, f.adminT, nil); code != 204 {
		t.Fatalf("delete = %d", code)
	}
	for _, a := range []string{"section.create", "section.update", "section.playlists", "section.delete"} {
		if f.audit(a, id) != 1 {
			t.Errorf("%s was not audited", a)
		}
	}
	// a failed change leaves no trace
	if code, _ = call(f.r, "PATCH", "/admin/playlist-groups/"+id, f.adminT, map[string]any{"title": "x"}); code != 404 {
		t.Fatalf("updating a deleted section = %d", code)
	}
	if f.audit("section.update", id) != 1 {
		t.Error("a refused change was audited")
	}
}
