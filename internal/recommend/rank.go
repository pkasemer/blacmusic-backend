package recommend

import (
	"cmp"
	"hash/fnv"
	"math"
	"slices"
	"strconv"
	"time"
)

// The two kinds of place in a list.
const (
	SlotFamiliar  = "familiar"
	SlotDiscovery = "discovery"
)

// Pick is a suggested song and why.
type Pick struct {
	TrackID  string             `json:"trackId"`
	ArtistID string             `json:"artistId"`
	Slot     string             `json:"slot"`
	Score    float64            `json:"score"`
	Reasons  []string           `json:"reasons"`
	Parts    map[string]float64 `json:"parts"` // what the score is made of
}

// Request is what a list is for.
type Request struct {
	Limit int
	// Seed, for a mix: the song it starts from. Only songs like it are offered.
	Seed *Track
	// PersonalOnly offers only songs with something of the listener's in them, never merely popular or new ones.
	PersonalOnly bool
	// PerArtist is the most one artist may have. Relax lets it be passed when nothing else is left.
	PerArtist int
	Relax     bool
	// Exclude are songs not to offer.
	Exclude map[string]bool
	// Variation changes the order from one list to the next; the same value gives the same list.
	Variation string
}

type scored struct {
	pick Pick
	// overall is what the song is ranked by in an ordinary place; asDiscovery in a place kept for a new artist, where
	// how widely a song is already heard counts the other way.
	overall, asDiscovery float64
}

func shared(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	both := 0
	for _, x := range a {
		if slices.Contains(b, x) {
			both++
		}
	}
	return float64(both) / float64(len(a)+len(b)-both)
}

// genreLikeness is 1 for a shared genre, part for a related one.
func genreLikeness(a, b Track) float64 {
	best := 0.0
	for _, g := range a.genreKeys() {
		if slices.Contains(b.genreKeys(), g) {
			return 1
		}
		for _, other := range related(g) {
			if slices.Contains(b.genreKeys(), other) {
				best = familyShare
			}
		}
	}
	return best
}

// likeness is how like the starting song another one is, 0..1, with what they share.
func likeness(seed, t Track) (float64, []string) {
	v, why := 0.0, []string{}
	add := func(x, weight float64, reason string) {
		if x > 0 {
			v += x * weight
			why = append(why, reason)
		}
	}
	if t.ArtistID == seed.ArtistID {
		add(1, simArtist, "same_artist")
	}
	add(genreLikeness(seed, t), simGenre, "similar_genre")
	if shared(seed.languages(), t.languages()) > 0 {
		add(1, simLanguage, "same_language")
	}
	add(shared(seed.Cultures, t.Cultures), simCulture, "same_culture")
	add(shared(seed.Regions, t.Regions), simRegion, "same_region")
	add(shared(seed.Attributes, t.Attributes), simSound, "same_sound")
	return v, why
}

// belongs says whether a song may be in a mix from seed. A song with a genre gets only songs of that genre or a related
// one: a mix is never padded with others. Without a genre, it takes the same artist, or a shared verified language,
// culture or region.
func belongs(seed, t Track) bool {
	if len(seed.genreKeys()) > 0 {
		return genreLikeness(seed, t) > 0
	}
	return t.ArtistID == seed.ArtistID || shared(seed.languages(), t.languages()) > 0 ||
		shared(seed.Cultures, t.Cultures) > 0 || shared(seed.Regions, t.Regions) > 0
}

func noise(variation, id string) float64 {
	h := fnv.New64a()
	h.Write([]byte(variation + "\x00" + id))
	return float64(h.Sum64()%100000)/100000 - 0.5
}

// Rank orders the candidates for a listener.
func Rank(p *Profile, candidates []Track, req Request, now time.Time) []Pick {
	_, pol := policyFor(p.Mode)
	most, artistListeners := 1, map[string]int{}
	for _, t := range candidates {
		most = max(most, t.Stats.Listeners30d+t.Stats.Listeners7d)
		artistListeners[t.ArtistID] += t.Stats.Listeners30d
	}
	mostArtist := 1
	for _, n := range artistListeners {
		mostArtist = max(mostArtist, n)
	}

	popularityWeight := wPopularity
	if p.Empty() {
		popularityWeight = wPopularityNewcomer
	}

	var familiar, discovery []scored
	seen := map[string]bool{}
	for _, t := range candidates {
		if req.Exclude[t.ID] || seen[t.ID] || (req.Seed != nil && t.ID == req.Seed.ID) {
			continue
		}
		seen[t.ID] = true
		if req.Seed != nil && !belongs(*req.Seed, t) {
			continue
		}
		h, known := p.History[t.ID]
		if known && h.avoided(now) {
			continue
		}

		parts, reasons := map[string]float64{}, []string{}
		part := func(name string, v float64, reason string, show float64) {
			if v != 0 {
				parts[name] = round(v)
			}
			if reason != "" && v > show {
				reasons = append(reasons, reason)
			}
		}
		artist := p.artist[t.ArtistID]
		why := "artist_you_play"
		switch {
		case p.favourites[t.ArtistID]:
			why = "favourite_artist"
		case p.followed[t.ArtistID]:
			why = "followed_artist"
		}
		part("artist", wArtist*artist, why, 0.03)
		genre := p.genreMatch(t)
		genreWhy := "genre_you_play"
		for _, g := range t.genreKeys() {
			if p.genresChosen[g] {
				genreWhy = "genre_you_chose"
			}
		}
		part("genre", wGenre*genre, genreWhy, 0.03)
		language, languageWhy := p.languageMatch(t)
		part("language", wLanguage*pol.languageWeight*language, languageWhy, 0.02)
		region, regionWhy := p.regionMatch(t, pol)
		part("region", wRegion*region, regionWhy, 0.01)
		sound := p.soundMatch(t)
		part("sound", wSound*sound, "similar_sound", 0.01)

		popularity := math.Log1p(float64(t.Stats.Listeners30d+t.Stats.Listeners7d)) / math.Log1p(float64(most))
		part("popularity", popularityWeight*popularity, "popular_now", 0.04)
		// the share of listeners who finish it rather than skip it, pulled to the middle while there are few
		finished := float64(t.Stats.Completes90d+2) / float64(t.Stats.Completes90d+t.Stats.Skips90d+4)
		part("quality", wQuality*(finished-0.5)*2, "", 0)
		age := math.Max(now.Sub(t.Released).Hours()/24, 0)
		freshness := math.Exp(-age / 60)
		part("fresh", wFresh*freshness, "new_release", 0.03)

		if known {
			if h.kept() {
				part("kept", bonusKept, "liked", 0)
			}
			if h.Completes >= repeatAfter || h.ReplaysW >= 1 {
				part("on_repeat", bonusOnRepeat, "on_repeat", 0)
			}
			if !h.LastHeard.IsZero() {
				days := now.Sub(h.LastHeard).Hours() / 24
				for _, step := range heardPenalty {
					if days < step.days {
						part("heard_lately", -step.penalty, "", 0)
						break
					}
				}
			}
			switch since := now.Sub(h.LastSkipped).Hours() / 24; {
			case !h.LastSkipped.IsZero() && since < 7:
				part("skipped_lately", -skippedThisWeek, "", 0)
			case !h.LastSkipped.IsZero() && since < 30:
				part("skipped_lately", -skippedThisMonth, "", 0)
			case !h.LastWeakSkipped.IsZero() && now.Sub(h.LastWeakSkipped).Hours() < 7*24:
				part("skipped_lately", -weakSkipThisWeek, "", 0)
			}
		}
		total := 0.0
		for _, name := range []string{"artist", "genre", "language", "region", "sound", "popularity", "quality", "fresh", "kept", "on_repeat", "heard_lately", "skipped_lately"} {
			total += parts[name]
		}
		match := math.Max(math.Max(genre, language), math.Max(region, sound))
		personal := artist > 0 || match > 0

		var alike float64
		if req.Seed != nil {
			var why []string
			alike, why = likeness(*req.Seed, t)
			parts["like_the_seed"] = round(alike)
			reasons = append(why, reasons...)
		}

		// the score shown is the overall one, for a mix with likeness to its starting song, so any two compare
		score := total
		if req.Seed != nil {
			score = mixSeedShare*alike + (1-mixSeedShare)*total
		}
		s := scored{pick: Pick{TrackID: t.ID, ArtistID: t.ArtistID, Slot: SlotFamiliar, Reasons: reasons, Parts: parts}, overall: score}
		isNew := p.newArtist(t.ArtistID)
		if isNew {
			s.pick.Slot = SlotDiscovery
			tradition := p.newTradition(t)
			if req.Seed != nil {
				match = math.Max(match, alike)
			}
			underHeard := 1 - float64(artistListeners[t.ArtistID])/float64(mostArtist)
			d := 0.45*match + 0.20*finished + 0.20*underHeard + 0.15*freshness + parts["heard_lately"] + parts["skipped_lately"]
			s.pick.Reasons = append(s.pick.Reasons, "new_artist_for_you")
			if underHeard > 0.8 {
				s.pick.Reasons = append(s.pick.Reasons, "under_discovered")
			}
			if tradition && pol.newTradition > 0 {
				d += pol.newTradition
				s.pick.Reasons = append(s.pick.Reasons, "new_tradition")
			}
			if req.Seed != nil {
				d = mixSeedShare*alike + (1-mixSeedShare)*d
			}
			s.asDiscovery = d
			parts["discovery_rank"] = round(d)
			// For "Your sound" a new artist's song has to match well enough, or be the new ground the mode asks for.
			// A mix, already held to its starting song, takes any.
			close := match > 0 && match >= pol.minMatch
			personal = close || (tradition && pol.newTradition > 0 && (match > 0 || pol.anyRegion))
		}
		if req.PersonalOnly && !personal {
			continue
		}
		s.pick.Score = round(score)
		// vary the order a little, without letting luck outweigh the reasons
		vary := jitter * noise(req.Variation, t.ID)
		s.overall += vary
		s.asDiscovery += vary
		if isNew {
			discovery = append(discovery, s)
		} else {
			familiar = append(familiar, s)
		}
	}
	return assemble(familiar, discovery, req, pol.discoveryShare)
}

func round(v float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', 4, 64), 64)
	return r
}

// assemble fills the list place by place: the discovery share is spread through it, one artist is held to PerArtist
// and kept apart from themselves, and when one kind runs out the other fills in.
func assemble(familiar, discovery []scored, req Request, share float64) []Pick {
	for _, lane := range [][]scored{familiar, discovery} {
		slices.SortFunc(lane, func(a, b scored) int { return cmp.Compare(a.pick.TrackID, b.pick.TrackID) })
	}
	perArtist := req.PerArtist
	if perArtist <= 0 {
		perArtist = max(2, int(math.Ceil(float64(req.Limit)*mixPerArtistShare)))
	}
	taken := map[string]int{}
	used := map[string]bool{}
	out := []Pick{}
	recent := func(artist string) bool {
		for i := len(out) - 1; i >= 0 && i >= len(out)-artistGap; i-- {
			if out[i].ArtistID == artist {
				return true
			}
		}
		return false
	}
	// best finds the highest in the lanes once each artist's earlier places count against them
	best := func(forDiscovery, spaced, capped bool, lanes ...[]scored) (Pick, bool) {
		var found Pick
		top, ok := math.Inf(-1), false
		for _, lane := range lanes {
			for _, s := range lane {
				a := s.pick.ArtistID
				if used[s.pick.TrackID] || (capped && taken[a] >= perArtist) || (spaced && recent(a)) {
					continue
				}
				v := s.overall
				if forDiscovery {
					v = s.asDiscovery
				}
				// taken off, not multiplied, so that it also lowers a score under zero
				if v -= (1 - math.Pow(artistRepeatFactor, float64(taken[a]))) * 0.5; v > top {
					found, top, ok = s.pick, v, true
				}
			}
		}
		return found, ok
	}
	for i := 0; len(out) < req.Limit; i++ {
		forNew := int(float64(i+1)*share+1e-9) > int(float64(i)*share+1e-9)
		var next Pick
		ok := false
		// apart from themselves if possible; then closer; then, if allowed, over the limit
		for _, try := range []struct{ spaced, capped bool }{{true, true}, {false, true}, {true, false}, {false, false}} {
			if !try.capped && !req.Relax {
				continue
			}
			if forNew {
				if next, ok = best(true, try.spaced, try.capped, discovery); !ok {
					next, ok = best(false, try.spaced, try.capped, familiar)
				}
			} else if next, ok = best(false, try.spaced, try.capped, familiar); !ok {
				// no more from artists they know: the best overall of the rest, not the least heard
				next, ok = best(false, try.spaced, try.capped, discovery)
			}
			if ok {
				break
			}
		}
		if !ok {
			break
		}
		used[next.TrackID] = true
		taken[next.ArtistID]++
		out = append(out, next)
	}
	return out
}
