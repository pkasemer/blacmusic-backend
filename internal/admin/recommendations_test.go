package admin

import (
	"testing"

	"github.com/blacmusic/backend/internal/testutil"
)

func TestLookingIntoRecommendations(t *testing.T) {
	f := setupFull(t)
	owner := testutil.User(t, f.h.pool, "user", true)
	art := testutil.Artist(t, f.h.pool, owner, testutil.Unique("Explain"), testutil.GiB)
	rel := testutil.Release(t, f.h.pool, art, "R", "single", "public")
	song, _ := testutil.ReadyTrack(t, f.h.pool, owner, art, rel, "S", 1, "public")
	listener := testutil.User(t, f.h.pool, "user", true)

	code, b := call(f.r, "GET", "/admin/users/"+listener+"/recommendations", f.adminT, nil)
	if code != 200 || b["algorithm"] != "hybrid-1" || b["discoveryMode"] != "explore_more" || len(b["items"].([]any)) != 0 {
		t.Fatalf("nothing known = %d %v", code, b)
	}
	if code, b := call(f.r, "GET", "/admin/users/"+listener+"/recommendations?surface=mix&seed="+song, f.adminT, nil); code != 200 {
		t.Fatalf("mix = %d %v", code, b)
	}
	for path, want := range map[string]int{
		"/admin/users/" + listener + "/recommendations?surface=mix&seed=trk_none": 422,
		"/admin/users/" + listener + "/recommendations?surface=radio":             400,
		"/admin/users/nobody/recommendations":                                     404,
		"/admin/recommendations/outcomes?days=30":                                 200,
	} {
		if code, b := call(f.r, "GET", path, f.adminT, nil); code != want {
			t.Errorf("%s = %d %v, want %d", path, code, b, want)
		}
	}
	code, b = call(f.r, "GET", "/admin/recommendations/readiness", f.adminT, nil)
	songs, _ := b["songs"].(map[string]any)
	if code != 200 || songs["playable"].(float64) < 1 {
		t.Fatalf("readiness = %d %v", code, b)
	}
	if code, _ := call(f.r, "GET", "/admin/recommendations/readiness", testutil.Token(t, listener), nil); code != 403 {
		t.Fatalf("listeners cannot look = %d", code)
	}
}
