package recommend

import (
	"math"
	"slices"
	"time"
)

// Track is what the engine knows about a song. Language, region, culture and sound are verified values only.
type Track struct {
	ID               string
	ArtistID         string
	Genre            string   // the main genre ("" when there is none)
	Genres           []string // the others on the song
	PrimaryLanguages []string
	OtherLanguages   []string
	Regions          []string
	Cultures         []string
	Attributes       []string // "mood:happy", "energy:high", ...
	Released         time.Time
	Stats            Stats
}

// Stats is how a song is doing with listeners as a whole (staff and test accounts left out).
type Stats struct {
	Listeners7d, Listeners30d, Completes90d, Skips90d, Savers int
}

func (t Track) genreKeys() []string {
	if t.Genre == "" {
		return t.Genres
	}
	if slices.Contains(t.Genres, t.Genre) {
		return t.Genres
	}
	return append([]string{t.Genre}, t.Genres...)
}

func (t Track) languages() []string {
	return append(slices.Clone(t.PrimaryLanguages), t.OtherLanguages...)
}

// History is one listener's history with one song (the rec_user_tracks view). The W values are decayed counts.
type History struct {
	Listens, Completes, Skips, WeakSkips, PlaylistAdds int
	CompletesW, ReplaysW, SkipsW, WeakSkipsW, PlaysW   float64
	Liked, Unliked, Saved                              bool
	LastHeard, LastSkipped, LastWeakSkipped            time.Time // zero when never
}

// engagement is how much a listener's history with a song says for it, or against it.
func (h History) engagement() float64 {
	v := h.CompletesW + 1.5*h.ReplaysW - h.SkipsW - 0.3*h.WeakSkipsW + 2*float64(min(h.PlaylistAdds, 2))
	if h.Liked {
		v += 3
	}
	if h.Saved {
		v += 2
	}
	if h.Unliked && !h.Saved {
		v -= 3
	}
	if h.Listens == 0 { // only counted plays are known
		v += 0.5 * h.PlaysW
	}
	return v
}

// kept is a song the listener holds on to.
func (h History) kept() bool { return h.Saved || h.Liked }

// avoided is a song not to offer: taken back, or passed over again and again.
func (h History) avoided(now time.Time) bool {
	if h.Unliked && !h.Saved {
		return true
	}
	if h.kept() || h.Completes > 0 || h.PlaylistAdds > 0 {
		return false
	}
	since := now.AddDate(0, 0, -avoidDays)
	recent := h.LastSkipped.After(since) || h.LastWeakSkipped.After(since)
	return recent && (h.Skips >= avoidSkips || h.Skips+h.WeakSkips >= avoidWeakSkips)
}

// Stated is what the listener told us. Nothing in it is worked out from anything else.
type Stated struct {
	Mode                string
	PreferredLanguages  []string
	UnderstoodLanguages []string
	Regions             []string
	Genres              []string
	FavouriteArtists    []string
	FollowedArtists     []string
}

// Profile is what the engine uses to rank for one listener. It is worked out for each request and not stored.
type Profile struct {
	Stated
	History map[string]History

	preferred, understood, regions, genresChosen, favourites, followed map[string]bool

	artist        map[string]float64 // -1..1
	genre         map[string]float64 // -1..1
	heardArtists  map[string]bool
	heardLanguage map[string]float64 // share of what they enjoy, 0..1
	heardRegion   map[string]float64
	sound         map[string]float64
	strength      float64 // how much behaviour there is to go on
}

func set(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// saturate turns a sum into 0..1 (or -1..0): the first few count most.
func saturate(v, half float64) float64 {
	if v < 0 {
		return v / (-v + half)
	}
	return v / (v + half)
}

// either combines two reasons to like something: each closes part of the remaining distance to 1.
func either(a, b float64) float64 { return 1 - (1-a)*(1-b) }

// NewProfile works out a listener's taste. songs holds the features of the songs in history that are still available.
func NewProfile(s Stated, history map[string]History, songs map[string]Track) *Profile {
	p := &Profile{
		Stated: s, History: history,
		preferred: set(s.PreferredLanguages), understood: set(s.UnderstoodLanguages), regions: set(s.Regions),
		genresChosen: set(s.Genres), favourites: set(s.FavouriteArtists), followed: set(s.FollowedArtists),
		artist: map[string]float64{}, genre: map[string]float64{}, heardArtists: map[string]bool{},
		heardLanguage: map[string]float64{}, heardRegion: map[string]float64{}, sound: map[string]float64{},
	}
	byArtist, byGenre := map[string]float64{}, map[string]float64{}
	enjoyed := 0.0
	ids := make([]string, 0, len(history))
	for id := range history {
		ids = append(ids, id)
	}
	slices.Sort(ids) // the same sums every time
	for _, id := range ids {
		t, ok := songs[id]
		if !ok {
			continue
		}
		e := history[id].engagement()
		p.heardArtists[t.ArtistID] = true
		byArtist[t.ArtistID] += e
		for _, g := range t.genreKeys() {
			byGenre[g] += e
		}
		if e <= 0 {
			continue
		}
		enjoyed += e
		for _, l := range t.languages() {
			p.heardLanguage[l] += e
		}
		for _, r := range t.Regions {
			p.heardRegion[r] += e
		}
		for _, a := range t.Attributes {
			p.sound[a] += e
		}
	}
	for _, m := range []map[string]float64{p.heardLanguage, p.heardRegion, p.sound} {
		for k := range m {
			// a few listens are not yet a taste
			m[k] = m[k] / math.Max(enjoyed, 8)
		}
	}
	p.strength = enjoyed
	// what was chosen at set-up matters most while there is little else to go on
	fresh := 1 / (1 + enjoyed/40)

	for id, v := range byArtist {
		p.artist[id] = saturate(v, 4)
	}
	for _, ids := range [][]string{s.FavouriteArtists, s.FollowedArtists} {
		for _, id := range ids {
			if _, ok := p.artist[id]; !ok {
				p.artist[id] = 0
			}
		}
	}
	for id, behaviour := range p.artist {
		prior := 0.0
		if p.favourites[id] {
			prior = 0.6 + 0.2*fresh
		}
		if p.followed[id] {
			prior = either(prior, 0.7)
		}
		if behaviour >= 0 {
			p.artist[id] = either(behaviour, prior)
		} else {
			p.artist[id] = behaviour + prior/2
		}
	}
	for id, v := range byGenre {
		p.genre[id] = saturate(v, 8)
	}
	for _, id := range s.Genres {
		prior := 0.4 + 0.4*fresh
		if v := p.genre[id]; v >= 0 {
			p.genre[id] = either(v, prior)
		} else {
			p.genre[id] = v + prior/2
		}
	}
	return p
}

// Empty is a listener we know nothing about.
func (p *Profile) Empty() bool {
	return len(p.History) == 0 && len(p.PreferredLanguages)+len(p.UnderstoodLanguages)+len(p.Regions)+len(p.Genres)+
		len(p.FavouriteArtists)+len(p.FollowedArtists) == 0
}

// genreMatch is how much the listener likes a song's genres, related genres counting in part.
func (p *Profile) genreMatch(t Track) float64 {
	best, worst := 0.0, 0.0
	for _, g := range t.genreKeys() {
		v := p.genre[g]
		worst = math.Min(worst, v)
		best = math.Max(best, v)
		for _, other := range related(g) {
			best = math.Max(best, familyShare*p.genre[other])
		}
	}
	if best > 0 {
		return best
	}
	return math.Max(worst, 0)
}

// languageMatch uses verified languages only. A language nobody chose counts for nothing, never against.
func (p *Profile) languageMatch(t Track) (v float64, why string) {
	better := func(x float64, reason string) {
		if x > v {
			v, why = x, reason
		}
	}
	for i, list := range [][]string{t.PrimaryLanguages, t.OtherLanguages} {
		for _, l := range list {
			if p.preferred[l] {
				better([]float64{preferredPrimary, preferredSecondary}[i], "preferred_language")
			}
			if p.understood[l] {
				better([]float64{understoodPrimary, understoodOther}[i], "understood_language")
			}
			better(math.Min(p.heardLanguage[l], 1)*heardLanguageMax, "language_you_play")
		}
	}
	return v, why
}

func (p *Profile) regionMatch(t Track, pol policy) (v float64, why string) {
	for _, r := range t.Regions {
		if pol.anyRegion {
			if v < 0.5 {
				v, why = 0.5, "ugandan_region"
			}
			continue
		}
		if p.regions[r] {
			return 1, "your_region"
		}
		if x := 0.6 * math.Min(p.heardRegion[r], 1); x > v {
			v, why = x, "region_you_play"
		}
	}
	return v, why
}

func (p *Profile) soundMatch(t Track) float64 {
	if len(t.Attributes) == 0 {
		return 0
	}
	sum := 0.0
	for _, a := range t.Attributes {
		sum += math.Min(p.sound[a], 1)
	}
	return sum / float64(len(t.Attributes))
}

// newTradition is a song in a verified language or region the listener has neither chosen nor met.
func (p *Profile) newTradition(t Track) bool {
	for _, l := range t.PrimaryLanguages {
		if !p.preferred[l] && !p.understood[l] && p.heardLanguage[l] == 0 {
			return true
		}
	}
	for _, r := range t.Regions {
		if !p.regions[r] && p.heardRegion[r] == 0 {
			return true
		}
	}
	return false
}

// newArtist is one the listener has no history with, has not picked and does not follow.
func (p *Profile) newArtist(id string) bool {
	return !p.heardArtists[id] && !p.favourites[id] && !p.followed[id]
}

func related(genre string) []string {
	for _, family := range genreFamilies {
		if slices.Contains(family, genre) {
			out := make([]string, 0, len(family)-1)
			for _, g := range family {
				if g != genre {
					out = append(out, g)
				}
			}
			return out
		}
	}
	return nil
}
