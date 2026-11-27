package lyrics

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits on what may be stored for one track.
const (
	MaxTextRunes = 20000
	MaxLines     = 2000
	MaxLineRunes = 500
	maxLRCRunes  = 100000 // timed lyrics as typed, translations included
	maxErrors    = 100
)

// Line is one timed line of lyrics. Translation is the English under it, if any; Text never includes it.
type Line struct {
	TimeMs      int    `json:"timeMs"`
	Text        string `json:"text"`
	Translation string `json:"translation,omitempty"`
}

// LineError is something wrong with one line of the lyrics as typed.
type LineError struct {
	Line    int    `json:"line"`
	Message string `json:"message"`
}

var (
	stampRe  = regexp.MustCompile(`^\[(\d{1,3}):(\d{2})(?:[.:](\d{1,3}))?\]`)
	likeRe   = regexp.MustCompile(`^\[\d`) // meant as a time stamp
	metaRe   = regexp.MustCompile(`^\[([A-Za-z#]+):([^\]]*)\]\s*$`)
	inlineRe = regexp.MustCompile(`<\d{1,3}:\d{1,2}(?:[.:]\d{1,3})?>`) // word-level time tags of "enhanced" LRC
)

func stampTime(m []string) (ms int, ok bool) {
	minutes, _ := strconv.Atoi(m[1])
	seconds, _ := strconv.Atoi(m[2])
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
	return (minutes*60+seconds)*1000 + frac, seconds <= 59
}

func formatStamp(ms int) string {
	if ms%10 != 0 {
		return fmt.Sprintf("[%02d:%02d.%03d]", ms/60000, ms/1000%60, ms%1000)
	}
	return fmt.Sprintf("[%02d:%02d.%02d]", ms/60000, ms/1000%60, ms%1000/10)
}

func normalizeNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// Format writes timed lines as they would be typed: each translation on the line under its lyric.
func Format(lines []Line) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(formatStamp(l.TimeMs) + l.Text + "\n")
		if l.Translation != "" {
			b.WriteString(l.Translation + "\n")
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// ParseLRC reads timed lyrics: lyric lines such as "[01:23.45]some words" (several time stamps may share one text),
// each optionally followed, on the very next line, by its translation without a stamp. An "[offset:+250]" tag
// (milliseconds; positive shows the lyrics sooner) is applied, other tags and word-level "<00:12.34>" tags are
// ignored. durationMs is the song's length, 0 when unknown. Everything wrong is reported, by line as typed, in a
// ValidationError. The result is sorted by time.
func ParseLRC(src string, durationMs int) ([]Line, error) {
	if utf8.RuneCountInString(src) > maxLRCRunes {
		return nil, &ValidationError{Msg: "the lyrics are too long"}
	}
	var wrong []LineError
	report := func(line int, msg string) {
		if len(wrong) < maxErrors {
			wrong = append(wrong, LineError{line, msg})
		}
	}
	type entry struct {
		times            []int
		text, translated string
		line             int
	}
	var entries []*entry
	var open *entry // the lyric line directly above, which a translation may follow
	offset, lastTime, lastLine, count := 0, -1, 0, 0
	for i, raw := range strings.Split(normalizeNewlines(strings.TrimPrefix(src, "\ufeff")), "\n") {
		n := i + 1
		s := strings.TrimSpace(raw)
		if s == "" {
			open = nil
			continue
		}
		if !stampRe.MatchString(s) {
			if m := metaRe.FindStringSubmatch(s); m != nil {
				if strings.EqualFold(m[1], "offset") {
					if v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(m[2], "+"))); err == nil {
						offset = v
					}
				}
				open = nil
				continue
			}
			if likeRe.MatchString(s) {
				report(n, fmt.Sprintf("Line %d: write the time as [mm:ss.xx], for example [01:07.50].", n))
				open = nil
				continue
			}
			text := strings.TrimSpace(inlineRe.ReplaceAllString(s, ""))
			switch {
			case open == nil:
				report(n, fmt.Sprintf("Line %d has no time stamp. A translation goes directly under its lyric line, one per line.", n))
			case open.text == "":
				report(n, fmt.Sprintf("Line %d: a music break has no translation.", n))
			case utf8.RuneCountInString(text) > MaxLineRunes:
				report(n, fmt.Sprintf("Line %d is longer than %d characters.", n, MaxLineRunes))
			default:
				open.translated = text
			}
			open = nil // one translation to a line
			continue
		}
		e := &entry{line: n}
		bad := false
		for {
			m := stampRe.FindStringSubmatch(s)
			if m == nil {
				break
			}
			t, ok := stampTime(m)
			bad = bad || !ok
			e.times = append(e.times, t)
			s = strings.TrimLeft(s[len(m[0]):], " \t")
		}
		if bad || likeRe.MatchString(s) {
			report(n, fmt.Sprintf("Line %d: write the time as [mm:ss.xx], for example [01:07.50].", n))
			open = e // its translation is not also an error
			e.text = "?"
			continue
		}
		e.text = strings.TrimSpace(inlineRe.ReplaceAllString(s, ""))
		if utf8.RuneCountInString(e.text) > MaxLineRunes {
			report(n, fmt.Sprintf("Line %d is longer than %d characters.", n, MaxLineRunes))
		}
		// lines with one time run forwards; a shared (repeated) line may point anywhere
		if len(e.times) == 1 {
			switch {
			case e.times[0] == lastTime:
				report(n, fmt.Sprintf("Line %d: A translation line has no time stamp.", n))
			case e.times[0] < lastTime:
				report(n, fmt.Sprintf("Line %d: its time must be after %s on line %d.", n, formatStamp(lastTime), lastLine))
			default:
				lastTime, lastLine = e.times[0], n
			}
		}
		entries, open = append(entries, e), e
		if count += len(e.times); count > MaxLines {
			return nil, &ValidationError{Msg: "too many lines (" + strconv.Itoa(MaxLines) + " at most)"}
		}
	}
	var lines []Line
	for _, e := range entries {
		late := false
		for _, t := range e.times {
			at := max(t-offset, 0)
			late = late || (durationMs > 0 && at > durationMs && e.text != "")
			lines = append(lines, Line{TimeMs: at, Text: e.text, Translation: e.translated})
		}
		if late {
			report(e.line, fmt.Sprintf("Line %d is after the song ends (%d:%02d).", e.line, durationMs/60000, durationMs/1000%60))
		}
	}
	if len(wrong) > 0 {
		sort.SliceStable(wrong, func(i, j int) bool { return wrong[i].Line < wrong[j].Line })
		return nil, &ValidationError{Msg: wrong[0].Message, Lines: wrong}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].TimeMs < lines[j].TimeMs })
	for len(lines) > 0 && lines[len(lines)-1].Text == "" { // no empty lines at the end
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil, &ValidationError{Msg: "no timed lines found: expected lines like [01:23.45]words"}
	}
	return lines, nil
}
