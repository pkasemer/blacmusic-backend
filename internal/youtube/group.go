package youtube

import (
	"sort"
	"time"

	"github.com/blacmusic/backend/internal/titles"
)

// Suggestion is the release a video would become.
type Suggestion struct {
	Title       string `json:"title"`
	ReleaseDate string `json:"releaseDate"`
	ReleaseType string `json:"releaseType"`
}

// VideoView is one song (or clip) as the app lists it.
type VideoView struct {
	VideoID         string     `json:"videoId"`
	Title           string     `json:"title"`
	PublishedAt     time.Time  `json:"publishedAt"`
	DurationSec     int        `json:"durationSec"`
	ThumbnailURL    string     `json:"thumbnailUrl,omitempty"`
	Kind            string     `json:"kind"` // song | short | promo
	GroupID         string     `json:"groupId"`
	Primary         bool       `json:"primary"`  // the video chosen to stand for the group
	Variants        []string   `json:"variants"` // the other videos of the same song
	Suggested       Suggestion `json:"suggested"`
	AlreadyImported bool       `json:"alreadyImported"`
	DuplicateOf     string     `json:"duplicateOf,omitempty"` // the catalog title it matches
}

// BuildViews groups the channel's videos by song. Audio uploads, lyric videos,
// visualizers, official videos and shorts of one song share a groupId; the best
// variant is the primary, and the release date suggested is the earliest upload
// of the group. owned maps normalised titles already in the artist's catalog.
func BuildViews(videos []Video, channel string, owned map[string]string) []VideoView {
	type member struct {
		v    Video
		kind string
		rank int
	}
	groups := map[string][]member{}
	var order []string
	for _, v := range videos {
		clean := titles.Clean(v.Title, channel)
		gid := titles.Norm(clean)
		if gid == "" {
			gid = v.ID
		}
		if _, ok := groups[gid]; !ok {
			order = append(order, gid)
		}
		groups[gid] = append(groups[gid], member{v: v, kind: titles.Kind(v.Title, v.DurationSec), rank: titles.VariantRank(v.Title)})
	}

	var out []VideoView
	for _, gid := range order {
		ms := groups[gid]
		// the primary is the best-ranked full-length video; shorts and promos only if nothing else exists
		sort.SliceStable(ms, func(i, j int) bool {
			ki, kj := ms[i].kind == "song", ms[j].kind == "song"
			if ki != kj {
				return ki
			}
			if ms[i].rank != ms[j].rank {
				return ms[i].rank < ms[j].rank
			}
			return ms[i].v.PublishedAt.Before(ms[j].v.PublishedAt)
		})
		earliest := ms[0].v.PublishedAt
		for _, m := range ms {
			if m.v.PublishedAt.Before(earliest) {
				earliest = m.v.PublishedAt
			}
		}
		variants := make([]string, 0, len(ms)-1)
		for _, m := range ms[1:] {
			variants = append(variants, m.v.ID)
		}
		p := ms[0]
		clean := titles.Clean(p.v.Title, channel)
		view := VideoView{
			VideoID: p.v.ID, Title: p.v.Title, PublishedAt: p.v.PublishedAt, DurationSec: p.v.DurationSec,
			ThumbnailURL: p.v.ThumbnailURL, Kind: p.kind, GroupID: gid, Primary: true, Variants: variants,
			Suggested: Suggestion{Title: clean, ReleaseDate: earliest.UTC().Format("2006-01-02"), ReleaseType: "single"},
		}
		if t, ok := owned[titles.Norm(clean)]; ok {
			view.AlreadyImported, view.DuplicateOf = true, t
		}
		out = append(out, view)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].PublishedAt.After(out[j].PublishedAt) })
	return out
}
