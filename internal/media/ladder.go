// Package media probes uploaded audio and turns it into the quality ladder.
package media

// Quality is a listening quality the app can request.
type Quality string

const (
	Low    Quality = "low"    // low-data mode
	Medium Quality = "medium" // standard listening (default)
	High   Quality = "high"
)

// Every rendition is AAC-LC in an MP4 container (.m4a) with the index at the
// front (faststart), so it starts playing as soon as the first bytes arrive and
// seeks with HTTP range requests on both Android and iOS.
const (
	Codec      = "aac"
	Container  = "m4a"
	MimeType   = "audio/mp4"
	SampleRate = 44100
)

// Rung is one step of the ladder.
type Rung struct {
	Quality     Quality
	BitrateKbps int
}

// Ladder is ordered from smallest to largest. Changing it only affects new
// uploads: each track's real bitrates are stored per rendition.
//
//	low     48 kbps  ~21 MB/hour  low-data mode
//	medium 128 kbps  ~58 MB/hour  standard listening
//	high   256 kbps  ~115 MB/hour near-transparent
var Ladder = []Rung{
	{Low, 48},
	{Medium, 128},
	{High, 256},
}

// Default is served when the client does not ask for a quality.
const Default = Medium

// ParseQuality maps a request value to a Quality. Empty means Default.
func ParseQuality(s string) (Quality, bool) {
	switch Quality(s) {
	case "":
		return Default, true
	case Low, Medium, High:
		return Quality(s), true
	}
	return "", false
}

func rank(q Quality) int {
	switch q {
	case Low:
		return 0
	case Medium:
		return 1
	}
	return 2
}

// Pick chooses which available quality to serve for a request: the exact one,
// otherwise the closest, preferring the lower (cheaper) quality on a tie.
func Pick(want Quality, have []Quality) (Quality, bool) {
	best, bestDist, found := Quality(""), 0, false
	for _, q := range have {
		d := rank(q) - rank(want)
		if d < 0 {
			d = -d
		}
		if !found || d < bestDist || (d == bestDist && rank(q) < rank(best)) {
			best, bestDist, found = q, d, true
		}
	}
	return best, found
}
