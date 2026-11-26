// Package titles holds the rules for recognising that two titles name the same
// song. The catalog importer, the YouTube discovery grouping and transfer
// duplicate checks all use them, so "same song" means the same thing everywhere.
package titles

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	featParen     = regexp.MustCompile(`(?i)[\(\[]\s*(feat\.?|ft\.?|featuring)[^\)\]]*[\)\]]`)
	featParenTail = regexp.MustCompile(`(?i)\s*[\(\[]\s*(feat\.?|ft\.?|featuring)[^\)\]]*[\)\]]\s*$`)
	featTail      = regexp.MustCompile(`(?i)\b(feat\.?|ft\.?|featuring)\b.*$`)
	nonAlnum      = regexp.MustCompile(`[^a-z0-9]+`)
	hashtag       = regexp.MustCompile(`#\w+`)
	bracketNote   = regexp.MustCompile(`(?i)[\(\[][^\)\]]*\b(official|lyrics?|lyrical|visuali[sz]er|audio|video|music video|mv|hd|4k)\b[^\)\]]*[\)\]]`)
	tailNote      = regexp.MustCompile(`(?i)[\s\-–—|:]*\b(official\s+(music\s+)?(video|audio|lyric\s+video)|lyrics?\s+video|lyric\s+video|lyrical\s+video|visuali[sz]er|full\s+audio|official\s+audio|audio)\b.*$`)
	tailWords     = regexp.MustCompile(`(?i)\s+(lyrics?(\s+shorts?)?|shorts?)\s*$`)
	spaces        = regexp.MustCompile(`\s+`)
)

// Norm reduces a title for comparison: case, punctuation and featured-artist
// credits do not matter ("Mukene (feat. Artin Pro)" equals "MUKENE").
func Norm(s string) string {
	s = strings.ToLower(s)
	s = featParen.ReplaceAllString(s, "")
	s = featTail.ReplaceAllString(s, "")
	s = nonAlnum.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// Clean turns a YouTube video title into a song title: it drops hashtags,
// "(Official Music Video)" style notes, "lyric video"/"visualizer" suffixes and a
// trailing " - CHANNEL NAME", and tidies ALL CAPS. channel may be empty.
func Clean(title, channel string) string {
	s := hashtag.ReplaceAllString(title, "")
	// keep a trailing "(feat. X)" credit through the clean-up
	credit := ""
	if loc := featParenTail.FindStringIndex(s); loc != nil {
		credit = strings.TrimSpace(s[loc[0]:])
		s = s[:loc[0]]
	}
	s = bracketNote.ReplaceAllString(s, "")
	s = tailNote.ReplaceAllString(s, "")
	s = tailWords.ReplaceAllString(s, "")
	if channel != "" {
		q := regexp.QuoteMeta(channel)
		// "Song - ARTIST ..." or "Song (Official Video)- ARTIST": cut from the separator before the channel name
		s = regexp.MustCompile(`(?i)\s*[\-–—|]\s*[^\-–—|]*\b`+q+`\b.*$`).ReplaceAllString(s, "")
		// "ARTIST - Song"
		s = regexp.MustCompile(`(?i)^\s*`+q+`\s*[\-–—|:]\s*`).ReplaceAllString(s, "")
	}
	s = strings.Trim(spaces.ReplaceAllString(s, " "), " -–—|:")
	if s == "" {
		s = strings.TrimSpace(title)
		credit = ""
	}
	if s == strings.ToUpper(s) && strings.IndexFunc(s, unicode.IsLetter) >= 0 {
		s = titleCase(s)
	}
	if credit != "" {
		s += " " + credit
	}
	return s
}

func titleCase(s string) string {
	words := strings.Fields(strings.ToLower(s))
	for i, w := range words {
		r := []rune(w)
		for j, c := range r {
			if unicode.IsLetter(c) {
				r[j] = unicode.ToUpper(c)
				break
			}
		}
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}

// Kind classifies a video: "song" (a candidate release), "short" (a vertical
// clip of a song) or "promo" (an announcement, teaser or behind-the-scenes clip).
func Kind(title string, durationSec int) string {
	l := strings.ToLower(title)
	switch {
	case strings.Contains(l, "#shorts") || (durationSec > 0 && durationSec <= 61):
		return "short"
	case containsAny(l, "out now", "stream and share", "keep streaming", "teaser", "snippet", "behind the scenes",
		"how we ", "trailer", "mash up", "mashup", "live session", "interview"):
		return "promo"
	}
	return "song"
}

// VariantRank orders the videos that carry one song: audio first, then
// visualizer, lyric video, official video, anything else.
func VariantRank(title string) int {
	l := strings.ToLower(title)
	switch {
	case containsAny(l, "full audio", "official audio", "(audio)", " audio"):
		return 0
	case strings.Contains(l, "visuali"):
		return 1
	case strings.Contains(l, "lyric"):
		return 2
	case strings.Contains(l, "official") || strings.Contains(l, "music video"):
		return 3
	}
	return 4
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
