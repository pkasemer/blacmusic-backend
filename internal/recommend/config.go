// Package recommend is Blac's hybrid recommendation engine. It ranks songs from what a listener said (onboarding),
// what they did (listening, likes, saves, playlists, follows), what the catalogue says about each song (verified
// metadata and genre) and how songs are doing with listeners as a whole. It does not use what similar listeners did
// (no collaborative filtering) or anything computed from the audio.
//
// Every number that shapes a ranking is in this file. Change them together with Version, so that what was suggested
// by one set of numbers can be told from another's.
package recommend

// Version is stored with everything suggested.
const Version = "hybrid-1"

// The discovery modes a listener can choose.
const (
	ModeFamiliar   = "familiar"
	ModeExplore    = "explore_more"
	ModeEverything = "everything_uganda"
)

// weights of the parts of a song's score. Each part is between 0 and 1 (quality between -1 and 1).
const (
	wArtist     = 0.28
	wGenre      = 0.20
	wLanguage   = 0.20
	wRegion     = 0.07
	wSound      = 0.05
	wPopularity = 0.08
	// for a listener nothing is known about, what others play is most of what there is to go on
	wPopularityNewcomer = 0.25
	wQuality            = 0.07
	wFresh              = 0.05
)

// What one song's own history adds or takes away.
const (
	bonusKept     = 0.08 // liked or saved
	bonusOnRepeat = 0.06 // finished several times, or played again straight away
	repeatAfter   = 3    // finishes that make a song one they come back to

	skippedThisWeek  = 0.50
	skippedThisMonth = 0.25
	weakSkipThisWeek = 0.10
)

// heardPenalty keeps what was just heard from coming straight back: age in days, then what it takes away.
var heardPenalty = []struct {
	days    float64
	penalty float64
}{{1, 0.60}, {3, 0.35}, {7, 0.20}, {14, 0.08}}

// A song is not offered at all while it was skipped at least this often in avoidDays, never finished, and is not kept.
// Skips from the app's first way of measuring need to be more, because some of them are false.
const (
	avoidSkips     = 2
	avoidWeakSkips = 3
	avoidDays      = 90
)

// policy is what a discovery mode changes.
type policy struct {
	discoveryShare float64 // of the list: songs by artists new to the listener
	minMatch       float64 // how well a new artist's song must match what the listener likes
	languageWeight float64 // times wLanguage
	newTradition   float64 // extra for a discovery in a verified language or region they have not met
	anyRegion      bool    // every Ugandan region counts alike, chosen or not
}

var policies = map[string]policy{
	ModeFamiliar:   {discoveryShare: 0.15, minMatch: 0.30, languageWeight: 1.2},
	ModeExplore:    {discoveryShare: 0.30, minMatch: 0.05, languageWeight: 1.0, newTradition: 0.15},
	ModeEverything: {discoveryShare: 0.45, minMatch: 0, languageWeight: 0.8, newTradition: 0.30, anyRegion: true},
}

// DefaultMode is used for a listener who has not chosen one.
const DefaultMode = ModeExplore

func policyFor(mode string) (string, policy) {
	if p, ok := policies[mode]; ok {
		return mode, p
	}
	return DefaultMode, policies[DefaultMode]
}

// Language: how much a song's verified language counts. Preferring a language and understanding it are different.
const (
	preferredPrimary   = 1.0
	preferredSecondary = 0.6
	understoodPrimary  = 0.4
	understoodOther    = 0.25
	heardLanguageMax   = 0.5 // learned from listening alone
)

// Related genres count for part of each other. Genre ids, or names for genres not in the list.
const familyShare = 0.6

var genreFamilies = [][]string{
	{"afrobeats", "afrobeat", "afropop"},
	{"rnb", "rnb-soul"},
	{"hip_hop", "rap", "trap", "drill"},
	{"electronic-dance", "dance", "house", "techno"},
	{"dancehall", "reggae"},
}

// How like the starting song of a mix another song is. The parts add up to 1.
const (
	simArtist   = 0.25
	simGenre    = 0.35
	simLanguage = 0.20
	simCulture  = 0.05
	simRegion   = 0.05
	simSound    = 0.10
	// mixSeedShare of a mix's ranking is likeness to the starting song; the rest is the listener.
	mixSeedShare = 0.55
)

// List shape.
const (
	artistRepeatFactor = 0.75 // each song already taken from an artist lowers their next one
	artistGap          = 2    // others between two songs of one artist, when there are others
	mixPerArtistShare  = 0.12 // of a mix, at most (never under 2); passed over only when nothing else is left
	soundPerArtist     = 4
	jitter             = 0.15 // how much the order varies from one list to the next
)
