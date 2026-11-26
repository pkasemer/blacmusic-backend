package playlist

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/blacmusic/backend/internal/testutil"
)

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Today's biggest hits": "todays-biggest-hits",
		"Workout":              "workout",
		"  Chill & Relax!! ":   "chill-relax",
		"Kampala  Nights":      "kampala-nights",
		"???":                  "",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func groupIDs(gs []Group) []string {
	out := []string{}
	for _, g := range gs {
		out = append(out, g.ID)
	}
	return out
}

func TestPlaylistGroups(t *testing.T) {
	d := setupDiscovery(t)
	ctx := context.Background()
	var ve *ValidationError

	// ids are unique per run so other data in the shared test database does not interfere
	mk := func(title string, pos int, active bool) string {
		g, err := d.store.CreateGroup(ctx, GroupInput{ID: Slug(testutil.Unique(title)), Title: title, Position: &pos, IsActive: &active})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.pool.Exec(ctx, `DELETE FROM playlist_groups WHERE id = $1`, g.ID) }) //nolint:errcheck
		return g.ID
	}
	// positions far above anything else keep these groups last and in a known order
	late := mk("Late", 9_000_002, true)
	early := mk("Early", 9_000_001, true)
	empty := mk("Empty", 9_000_003, true)
	hidden := mk("Hidden", 9_000_004, false)
	mine := func(gs []Group) []string {
		out := []string{}
		for _, id := range groupIDs(gs) {
			if slices.Contains([]string{late, early, empty, hidden}, id) {
				out = append(out, id)
			}
		}
		return out
	}

	// membership: replaces members and order; a playlist may be in many groups
	g, err := d.store.SetGroupPlaylists(ctx, early, []string{d.fresh, d.pub, d.private, d.draftOnly})
	if err != nil || !slices.Equal(g.PlaylistIDs, []string{d.fresh, d.pub, d.private, d.draftOnly}) || g.PlaylistCount != 2 {
		t.Fatalf("early members = %+v %v (private and draft-only are members but not listed)", g, err)
	}
	if _, err := d.store.SetGroupPlaylists(ctx, late, []string{d.pub}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.store.SetGroupPlaylists(ctx, hidden, []string{d.pub}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.store.SetGroupPlaylists(ctx, empty, []string{d.private}); err != nil {
		t.Fatal(err)
	}

	// what listeners see: active groups with a listed playlist, by position
	gs, err := d.store.ListGroups(ctx, d.fan, 50, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(mine(gs), []string{early, late}) {
		t.Fatalf("groups = %v, want [early late] (empty-of-listed and inactive hidden)", mine(gs))
	}
	var first Group
	for _, x := range gs {
		if x.ID == early {
			first = x
		}
	}
	if first.PlaylistCount != 2 || len(first.Playlists) != 2 || first.Playlists[0].ID != d.fresh || first.Playlists[1].ID != d.pub ||
		first.Playlists[0].Owner.ID != d.owner || first.Title != "Early" {
		t.Fatalf("early shelf = %+v", first)
	}
	// playlistsPerGroup trims the shelf but not the count; limit trims the groups
	if gs, _ = d.store.ListGroups(ctx, d.fan, 50, 1); len(gs) == 0 {
		t.Fatal("no groups")
	}
	for _, x := range gs {
		if x.ID == early && (len(x.Playlists) != 1 || x.PlaylistCount != 2) {
			t.Fatalf("perGroup = %d, count %d", len(x.Playlists), x.PlaylistCount)
		}
	}
	if gs, _ = d.store.ListGroups(ctx, d.fan, 1, 10); len(gs) > 1 {
		t.Fatalf("limit 1 returned %d groups", len(gs))
	}

	// the See all page
	pg, err := d.store.GetGroup(ctx, d.fan, early, 1, 1)
	if err != nil || pg.PlaylistCount != 2 || len(pg.Playlists) != 1 || pg.Playlists[0].ID != d.pub {
		t.Fatalf("page 2 = %+v %v", pg, err)
	}
	if _, err := d.store.GetGroup(ctx, d.fan, hidden, 20, 0); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("inactive group = %v", err)
	}
	if _, err := d.store.GetGroup(ctx, d.fan, "no-such-group", 20, 0); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("unknown group = %v", err)
	}

	// replacing membership changes order; unknown or repeated ids are refused and change nothing
	if g, err = d.store.SetGroupPlaylists(ctx, early, []string{d.pub, d.fresh}); err != nil || !slices.Equal(g.PlaylistIDs, []string{d.pub, d.fresh}) {
		t.Fatalf("reorder = %+v %v", g, err)
	}
	if _, err := d.store.SetGroupPlaylists(ctx, early, []string{d.pub, "pl_nope"}); !errors.As(err, &ve) {
		t.Fatalf("unknown playlist = %v", err)
	}
	if _, err := d.store.SetGroupPlaylists(ctx, early, []string{d.pub, d.pub}); !errors.As(err, &ve) {
		t.Fatalf("repeated playlist = %v", err)
	}
	if g, _ = d.store.AdminGetGroup(ctx, early); !slices.Equal(g.PlaylistIDs, []string{d.pub, d.fresh}) {
		t.Fatalf("a refused change altered the group: %v", g.PlaylistIDs)
	}
	if _, err := d.store.SetGroupPlaylists(ctx, "no-such-group", nil); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("unknown group members = %v", err)
	}

	// a playlist that goes private disappears from the shelf and is a member again when it returns
	priv := false
	if _, err := d.store.Update(ctx, d.pub, d.owner, Patch{IsPublic: &priv}); err != nil {
		t.Fatal(err)
	}
	if pg, _ = d.store.GetGroup(ctx, d.fan, early, 20, 0); pg.PlaylistCount != 1 || pg.Playlists[0].ID != d.fresh {
		t.Fatalf("after going private: %+v", pg)
	}
	if gs, _ = d.store.ListGroups(ctx, d.fan, 50, 10); slices.Contains(mine(gs), late) {
		t.Fatal("a group whose only playlist went private must disappear")
	}

	// editing
	title, pos, off := "Late Night", 9_000_000, false
	up, err := d.store.UpdateGroup(ctx, early, GroupPatch{Title: &title, Position: &pos, IsActive: &off})
	if err != nil || up.Title != title || up.Position != pos || up.IsActive {
		t.Fatalf("update = %+v %v", up, err)
	}
	if _, err := d.store.UpdateGroup(ctx, early, GroupPatch{Title: ref("")}); !errors.As(err, &ve) {
		t.Fatalf("empty title = %v", err)
	}
	if _, err := d.store.UpdateGroup(ctx, "no-such-group", GroupPatch{Title: &title}); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("unknown group update = %v", err)
	}
	all, _ := d.store.AdminListGroups(ctx)
	seen := 0
	for _, a := range all {
		if a.ID == early || a.ID == hidden {
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("admin list must include inactive groups, saw %d", seen)
	}

	// create: slug default, duplicate, bad id
	g, err = d.store.CreateGroup(ctx, GroupInput{Title: testutil.Unique("Brand New Shelf")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.pool.Exec(ctx, `DELETE FROM playlist_groups WHERE id = $1`, g.ID) }) //nolint:errcheck
	if g.ID != Slug(g.Title) || !g.IsActive || g.Position < 1 {
		t.Fatalf("defaults = %+v", g)
	}
	if _, err := d.store.CreateGroup(ctx, GroupInput{ID: g.ID, Title: "again"}); !errors.Is(err, ErrGroupExists) {
		t.Fatalf("duplicate = %v", err)
	}
	if _, err := d.store.CreateGroup(ctx, GroupInput{ID: "Bad Id!", Title: "x"}); !errors.As(err, &ve) {
		t.Fatalf("bad id = %v", err)
	}
	if _, err := d.store.CreateGroup(ctx, GroupInput{Title: "  "}); !errors.As(err, &ve) {
		t.Fatalf("blank title = %v", err)
	}

	// deleting a group keeps its playlists
	if err := d.store.DeleteGroup(ctx, late); err != nil {
		t.Fatal(err)
	}
	if err := d.store.DeleteGroup(ctx, late); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	if _, err := d.store.Get(ctx, d.fresh, d.owner, 10, 0); err != nil {
		t.Fatalf("playlist gone after deleting a group: %v", err)
	}
}
