package recommend

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

func song(id, artist, genre string, change ...func(*Track)) Track {
	t := Track{ID: id, ArtistID: artist, Genre: genre, Released: now.AddDate(-1, 0, 0)}
	for _, c := range change {
		c(&t)
	}
	return t
}

func lang(l ...string) func(*Track)   { return func(t *Track) { t.PrimaryLanguages = l } }
func region(r ...string) func(*Track) { return func(t *Track) { t.Regions = r } }
func daysAgo(d float64) time.Time     { return now.Add(-time.Duration(d * 24 * float64(time.Hour))) }
func profile(s Stated) *Profile       { return NewProfile(s, nil, nil) }
func ids(picks []Pick) (out []string) {
	for _, p := range picks {
		out = append(out, p.TrackID)
	}
	return out
}

// still ranks without the variation between lists.
func still(p *Profile, c []Track, req Request) []Pick {
	req.Variation = "test"
	if req.Limit == 0 {
		req.Limit = 25
	}
	return Rank(p, c, req, now)
}

func scoreOf(picks []Pick, id string) float64 {
	for _, p := range picks {
		if p.TrackID == id {
			return p.Score
		}
	}
	return -99
}

func TestLanguagePreferenceAndComprehensionAreDifferentAndNothingCountsAgainst(t *testing.T) {
	p := profile(Stated{PreferredLanguages: []string{"luganda"}, UnderstoodLanguages: []string{"english"}, FollowedArtists: []string{"x"}})
	got := still(p, []Track{
		song("preferred", "x", "", lang("luganda")),
		song("understood", "x", "", lang("english")),
		song("other", "x", "", lang("acholi")),
		song("unlabelled", "x", ""),
	}, Request{PerArtist: 10})
	pref, under, other, none := scoreOf(got, "preferred"), scoreOf(got, "understood"), scoreOf(got, "other"), scoreOf(got, "unlabelled")
	if !(pref > under && under > other && other == none) {
		t.Fatalf("preferred %v, understood %v, another language %v, none %v", pref, under, other, none)
	}
	if !slices.Contains(got[0].Reasons, "preferred_language") || got[0].TrackID != "preferred" {
		t.Fatalf("first = %+v", got[0])
	}
}

func TestTasteComesFromWhatPeopleDo(t *testing.T) {
	songs := map[string]Track{"loved": song("loved", "a", "gospel"), "passed": song("passed", "b", "trap")}
	p := NewProfile(Stated{}, map[string]History{
		"loved":  {Listens: 6, Completes: 5, CompletesW: 5, Liked: true, LastHeard: daysAgo(20)},
		"passed": {Listens: 4, Skips: 4, SkipsW: 4, LastHeard: daysAgo(20), LastSkipped: daysAgo(100)},
	}, songs)
	got := still(p, []Track{song("a2", "a", "gospel"), song("b2", "b", "trap"), song("c1", "c", "gospel"), song("d1", "d", "rock")}, Request{PerArtist: 5})
	if !(scoreOf(got, "a2") > scoreOf(got, "d1") && scoreOf(got, "d1") > scoreOf(got, "b2")) {
		t.Fatalf("liked artist over unknown over passed over: %v", got)
	}
	if p.genre["gospel"] <= 0 || p.genre["trap"] >= 0 || p.artist["a"] <= 0 || p.artist["b"] >= 0 {
		t.Fatalf("taste = %v %v", p.genre, p.artist)
	}
}

func TestSkipsAndRecentPlays(t *testing.T) {
	c := []Track{song("fresh", "a", "pop"), song("skipped", "b", "pop"), song("heard", "c", "pop"), song("twice", "d", "pop"),
		song("oldway2", "e", "pop"), song("oldway3", "f", "pop"), song("twiceLiked", "g", "pop"), song("takenBack", "h", "pop")}
	byID := map[string]Track{}
	for _, s := range c {
		byID[s.ID] = s
	}
	p := NewProfile(Stated{Genres: []string{"pop"}}, map[string]History{
		"skipped":    {Listens: 1, Skips: 1, SkipsW: 1, LastHeard: daysAgo(2), LastSkipped: daysAgo(2)},
		"heard":      {Listens: 1, Completes: 1, CompletesW: 1, LastHeard: daysAgo(0.1)},
		"twice":      {Listens: 2, Skips: 2, SkipsW: 2, LastSkipped: daysAgo(40)},
		"oldway2":    {Listens: 2, WeakSkips: 2, WeakSkipsW: 2, LastWeakSkipped: daysAgo(40)},
		"oldway3":    {Listens: 3, WeakSkips: 3, WeakSkipsW: 3, LastWeakSkipped: daysAgo(40)},
		"twiceLiked": {Listens: 2, Skips: 2, SkipsW: 2, Liked: true, LastSkipped: daysAgo(40)},
		"takenBack":  {Unliked: true},
	}, byID)
	got := still(p, c, Request{})
	in := ids(got)
	for _, gone := range []string{"twice", "oldway3", "takenBack"} {
		if slices.Contains(in, gone) {
			t.Errorf("%s should not be offered: %v", gone, in)
		}
	}
	// two skips from the first way of measuring may be false ones, and a liked song stays
	for _, kept := range []string{"oldway2", "twiceLiked"} {
		if !slices.Contains(in, kept) {
			t.Errorf("%s should still be offered: %v", kept, in)
		}
	}
	if !(scoreOf(got, "fresh") > scoreOf(got, "heard") && scoreOf(got, "fresh") > scoreOf(got, "skipped")) {
		t.Errorf("what was just heard or skipped comes later: %v", got)
	}
}

// catalogue has songs the listener's artists made and songs by artists new to them, all in a chosen genre.
func catalogue() []Track {
	var c []Track
	for a := range 10 {
		for i := range 4 {
			c = append(c, song(fmt.Sprintf("known%d-%d", a, i), fmt.Sprintf("known%d", a), "gospel"))
			c = append(c, song(fmt.Sprintf("new%d-%d", a, i), fmt.Sprintf("new%d", a), "gospel"))
		}
	}
	return c
}

func followedKnown(mode string) *Profile {
	var f []string
	for a := range 10 {
		f = append(f, fmt.Sprintf("known%d", a))
	}
	return profile(Stated{Mode: mode, FollowedArtists: f, Genres: []string{"gospel"}})
}

func TestDiscoveryModeSetsHowMuchIsNewAndSpreadsIt(t *testing.T) {
	for mode, want := range map[string]int{ModeFamiliar: 3, ModeExplore: 6, ModeEverything: 9, "": 6} {
		got := still(followedKnown(mode), catalogue(), Request{Limit: 20})
		n, firstTen := 0, 0
		for i, p := range got {
			if p.Slot == SlotDiscovery {
				n++
				if i < 10 {
					firstTen++
				}
				if !slices.Contains(p.Reasons, "new_artist_for_you") {
					t.Fatalf("a discovery says so: %v", p.Reasons)
				}
			}
		}
		if len(got) != 20 || n != want || firstTen == 0 || firstTen == n && n > 5 {
			t.Errorf("mode %q: %d songs, %d new (%d in the first ten), want %d", mode, len(got), n, firstTen, want)
		}
	}
}

func TestEverythingUgandaReachesOtherTraditionsAndStopsFavouringChosenRegions(t *testing.T) {
	c := []Track{
		song("home", "a", "gospel", region("central_uganda"), lang("luganda")),
		song("north", "b", "gospel", region("northern_uganda"), lang("acholi")),
	}
	stated := Stated{Regions: []string{"central_uganda"}, PreferredLanguages: []string{"luganda"}, Genres: []string{"gospel"}}
	stated.Mode = ModeFamiliar
	if got := still(profile(stated), c, Request{}); got[0].TrackID != "home" {
		t.Fatalf("familiar = %v", ids(got))
	}
	stated.Mode = ModeEverything
	got := still(profile(stated), c, Request{})
	var north Pick
	for _, p := range got {
		if p.TrackID == "north" {
			north = p
		}
		if p.Parts["region"] != round(wRegion*0.5) {
			t.Fatalf("every Ugandan region alike: %+v", p)
		}
	}
	if !slices.Contains(north.Reasons, "new_tradition") {
		t.Fatalf("north = %+v", north)
	}
}

func TestOneArtistDoesNotFillAList(t *testing.T) {
	var c []Track
	for i := range 30 {
		c = append(c, song(fmt.Sprintf("big%d", i), "big", "gospel"))
	}
	for a := range 12 {
		c = append(c, song(fmt.Sprintf("a%d-1", a), fmt.Sprintf("a%d", a), "gospel"), song(fmt.Sprintf("a%d-2", a), fmt.Sprintf("a%d", a), "gospel"))
	}
	p := profile(Stated{FavouriteArtists: []string{"big"}, Genres: []string{"gospel"}})
	got := still(p, c, Request{Limit: 25, Relax: true})
	count := 0
	for i, x := range got {
		if x.ArtistID == "big" {
			count++
		}
		for j := max(0, i-artistGap); j < i; j++ {
			if got[j].ArtistID == x.ArtistID {
				t.Fatalf("%s sits too close to itself at %d: %v", x.ArtistID, i, ids(got))
			}
		}
	}
	if len(got) != 25 || count != 3 {
		t.Fatalf("%d songs, %d by the favourite", len(got), count)
	}
	// with nothing else, a mix is still filled; "Your sound" is not
	only := c[:30]
	if got := still(p, only, Request{Limit: 10, Relax: true}); len(got) != 10 {
		t.Fatalf("relaxed = %d", len(got))
	}
	if got := still(p, only, Request{Limit: 10, PerArtist: soundPerArtist}); len(got) != soundPerArtist {
		t.Fatalf("held = %d", len(got))
	}
}

func TestAMixKeepsToItsStartingSong(t *testing.T) {
	seed := song("seed", "s", "afrobeats", lang("luganda"))
	c := []Track{seed, song("same", "a", "afrobeats"), song("sameLuganda", "b", "afrobeats", lang("luganda")), song("related", "c", "afrobeat"),
		song("otherGenre", "s", "gospel", lang("luganda")), song("none", "d", "")}
	got := still(profile(Stated{Genres: []string{"gospel"}}), c, Request{Seed: &seed, Relax: true})
	if want := []string{"sameLuganda", "same", "related"}; !slices.Equal(ids(got), want) {
		t.Fatalf("mix = %v, want %v", ids(got), want)
	}
	if !slices.Contains(got[0].Reasons, "same_language") || !slices.Contains(got[0].Reasons, "similar_genre") {
		t.Fatalf("reasons = %v", got[0].Reasons)
	}
	bare := song("bare", "s", "")
	got = still(profile(Stated{}), []Track{bare, song("sameArtist", "s", "gospel"), song("stranger", "z", "gospel")}, Request{Seed: &bare})
	if !slices.Equal(ids(got), []string{"sameArtist"}) {
		t.Fatalf("a song without a genre = %v", ids(got))
	}
}

func TestYourSoundNeedsSomethingOfTheListeners(t *testing.T) {
	c := catalogue()
	c[0].Stats = Stats{Listeners30d: 500, Listeners7d: 100}
	if got := still(profile(Stated{}), c, Request{PersonalOnly: true}); len(got) != 0 {
		t.Fatalf("nothing known = %v", ids(got))
	}
	// a newcomer's mix is not empty: what is popular leads
	if got := still(profile(Stated{}), c, Request{Limit: 5}); len(got) != 5 || !slices.Contains(ids(got), c[0].ID) {
		t.Fatalf("cold start = %v", ids(got))
	}
	got := still(profile(Stated{Genres: []string{"gospel"}}), append(c, song("rock", "r", "rock")), Request{PersonalOnly: true, Limit: 20})
	if len(got) != 20 || slices.Contains(ids(got), "rock") {
		t.Fatalf("chosen genre = %v", ids(got))
	}
}

func TestSameRequestSameListAndVariationChangesIt(t *testing.T) {
	p, c := followedKnown(ModeExplore), catalogue()
	a := ids(Rank(p, c, Request{Limit: 20, Variation: "monday"}, now))
	if !slices.Equal(a, ids(Rank(p, c, Request{Limit: 20, Variation: "monday"}, now))) {
		t.Fatal("the same variation must give the same list")
	}
	if slices.Equal(a, ids(Rank(p, c, Request{Limit: 20, Variation: "tuesday"}, now))) {
		t.Fatal("another variation should differ")
	}
}
