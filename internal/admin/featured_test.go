package admin

import (
	"context"
	"slices"
	"testing"

	"github.com/blacmusic/backend/internal/testutil"
)

func TestFeaturedReleases(t *testing.T) {
	w := newWorld(t)
	art := w.catalogArtist("Featured Artist")
	pool := w.h.pool
	mk := func(typ, visibility string) string {
		rel := testutil.Release(t, pool, art, "F", typ, visibility)
		testutil.ReadyTrack(t, pool, w.admin, art, rel, "T", 1, visibility)
		return rel
	}
	first, second, hidden, single := mk("album", "public"), mk("album", "public"), mk("album", "draft"), mk("single", "public")
	// other tests' leftovers must not disturb the order
	w.exec(`UPDATE albums SET featured_position = NULL`)
	t.Cleanup(func() { w.exec(`UPDATE albums SET featured_position = NULL WHERE artist_id = $1`, art) })
	set := func(method, id string) (int, map[string]any) {
		return call(w.r, method, "/admin/releases/"+id+"/featured", w.adminT, nil)
	}
	// what listeners get
	shown := func() []string {
		t.Helper()
		list, err := w.rel.Browse(context.Background(), "featured", "", 50, 0)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, r := range list {
			out = append(out, r.ID)
		}
		return out
	}

	if len(shown()) != 0 {
		t.Fatal("nothing is featured yet")
	}
	for _, id := range []string{first, hidden, second, first} {
		if code, got := set("PUT", id); code != 200 || got["featured"] != true {
			t.Fatalf("feature = %d %v", code, got)
		}
	}
	if w.audit("release.feature", first) != 1 {
		t.Fatal("featuring is recorded once")
	}
	if code, got := set("PUT", single); code != 409 || errCode(got) != "not_eligible" {
		t.Fatalf("single = %d %v", code, got)
	}
	if code, _ := set("PUT", "alb_nothing"); code != 404 {
		t.Fatalf("unknown = %d", code)
	}
	if got := shown(); !slices.Equal(got, []string{first, second}) {
		t.Fatalf("shown = %v (a draft is hidden)", got)
	}

	order := func(ids ...string) int {
		code, _ := call(w.r, "PUT", "/admin/featured-releases/order", w.adminT, map[string]any{"releaseIds": ids})
		return code
	}
	if code := order(second, first); code != 422 {
		t.Fatalf("an incomplete order = %d", code)
	}
	if code := order(second, hidden, first); code != 204 {
		t.Fatalf("order = %d", code)
	}
	if got := shown(); !slices.Equal(got, []string{second, first}) {
		t.Fatalf("reordered = %v", got)
	}
	code, b := call(w.r, "GET", "/admin/releases?featured=true", w.adminT, nil)
	var listed []string
	if code == 200 {
		for _, r := range b["releases"].([]any) {
			listed = append(listed, r.(map[string]any)["id"].(string))
		}
	}
	if !slices.Equal(listed, []string{second, hidden, first}) {
		t.Fatalf("admin list = %d %v", code, listed)
	}
	if code, got := set("DELETE", second); code != 200 || got["featured"] != false {
		t.Fatalf("unfeature = %d %v", code, got)
	}
	if got := shown(); !slices.Equal(got, []string{first}) || w.audit("release.unfeature", second) != 1 {
		t.Fatalf("after unfeature = %v", got)
	}
}
