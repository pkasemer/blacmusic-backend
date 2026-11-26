package lyrics

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits on what an artist may store for one track.
const (
	MaxTextRunes = 20000
	MaxLines     = 2000
	MaxLineRunes = 500
)

// Line is one timed line of lyrics.
type Line struct {
	TimeMs int    `json:"timeMs"`
	Text   string `json:"text"`
}

var (
	stampRe  = regexp.MustCompile(`^\[(\d{1,3}):(\d{1,2})(?:[.:](\d{1,3}))?\]`)
	metaRe   = regexp.MustCompile(`^\[([A-Za-z#]+):([^\]]*)\]\s*$`)
	inlineRe = regexp.MustCompile(`<\d{1,3}:\d{1,2}(?:[.:]\d{1,3})?>`) // word-level time tags of "enhanced" LRC
)

// ParseLRC reads standard LRC: lines such as "[01:23.45]some words", several time stamps in front of one
// text, an optional "[offset:+250]" tag (milliseconds; a positive offset shows the lyrics sooner), other
// tags such as [ar:..] [ti:..] ignored, word-level "<00:12.34>" tags dropped. Lines without a time stamp
// are skipped. The result is sorted by time.
func ParseLRC(src string) ([]Line, error) {
	src = strings.ReplaceAll(strings.ReplaceAll(src, "\r\n", "\n"), "\r", "\n")
	offset := 0
	var lines []Line
	for _, raw := range strings.Split(src, "\n") {
		s := strings.TrimSpace(raw)
		if s == "" || (!stampRe.MatchString(s) && !metaRe.MatchString(s)) {
			continue
		}
		if m := metaRe.FindStringSubmatch(s); m != nil && !stampRe.MatchString(s) {
			if strings.EqualFold(m[1], "offset") {
				if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(m[2], "+"))); err == nil {
					offset = n
				}
			}
			continue
		}
		var times []int
		for {
			m := stampRe.FindStringSubmatch(s)
			if m == nil {
				break
			}
			min, _ := strconv.Atoi(m[1])
			sec, _ := strconv.Atoi(m[2])
			if sec > 59 {
				return nil, &ValidationError{"a time stamp has more than 59 seconds: " + m[0]}
			}
			frac := 0
			if m[3] != "" {
				frac, _ = strconv.Atoi(m[3])
				switch len(m[3]) {
				case 1:
					frac *= 100
				case 2:
					frac *= 10
				}
			}
			times = append(times, (min*60+sec)*1000+frac)
			s = strings.TrimLeft(s[len(m[0]):], " \t")
		}
		text := strings.TrimSpace(inlineRe.ReplaceAllString(s, ""))
		if utf8.RuneCountInString(text) > MaxLineRunes {
			return nil, &ValidationError{"a line is longer than " + strconv.Itoa(MaxLineRunes) + " characters"}
		}
		for _, t := range times {
			lines = append(lines, Line{TimeMs: max(t-offset, 0), Text: text})
		}
		if len(lines) > MaxLines {
			return nil, &ValidationError{"too many lines (" + strconv.Itoa(MaxLines) + " at most)"}
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].TimeMs < lines[j].TimeMs })
	for len(lines) > 0 && lines[len(lines)-1].Text == "" { // no empty lines at the end
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil, &ValidationError{"no timed lines found: expected lines like [01:23.45]words"}
	}
	return lines, nil
}
