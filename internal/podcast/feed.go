// Package podcast ingests RSS podcast feeds and serves shows, episodes,
// subscriptions and listening progress.
package podcast

import (
	"bytes"
	"encoding/xml"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
)

const (
	// MaxEpisodesPerFeed bounds how many episodes (newest first) are kept per show.
	MaxEpisodesPerFeed = 2000

	maxTitleRunes       = 300
	maxShowDescRunes    = 2000
	maxEpisodeDescRunes = 4000
	maxURLLen           = 2048
	maxDurationMs       = 48 * 60 * 60 * 1000

	itunesNS  = "http://www.itunes.com/dtds/podcast-1.0.dtd"
	contentNS = "http://purl.org/rss/1.0/modules/content/"
)

// FeedError means the feed itself is unusable. Its message is safe to show users.
type FeedError struct{ Msg string }

func (e *FeedError) Error() string { return e.Msg }

// Feed is a parsed podcast feed.
type Feed struct {
	Title       string
	Author      string
	Description string
	ImageURL    string
	Link        string
	Language    string
	Explicit    bool
	Episodes    []Episode // newest first, at most MaxEpisodesPerFeed
}

// Episode is one item of a feed that has playable audio.
type Episode struct {
	GUID          string
	Title         string
	Description   string
	AudioURL      string
	MimeType      string
	SizeBytes     int64
	DurationMs    int
	PublishedAt   time.Time // zero when the feed gave no usable date
	Season        *int
	EpisodeNumber *int
	EpisodeType   string
	ImageURL      string
	Explicit      bool
}

type xmlImage struct {
	Href string `xml:"href,attr"`
	URL  string `xml:"url"`
}

type xmlChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Language    string    `xml:"language"`
	Author      string    `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd author"`
	Summary     string    `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd summary"`
	ItunesImage xmlImage  `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd image"`
	Image       xmlImage  `xml:"image"`
	Explicit    string    `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd explicit"`
	Items       []xmlItem `xml:"item"`
}

type xmlItem struct {
	Title       string `xml:"title"`
	GUID        string `xml:"guid"`
	Description string `xml:"description"`
	Content     string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
	Summary     string `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd summary"`
	PubDate     string `xml:"pubDate"`
	Enclosure   struct {
		URL    string `xml:"url,attr"`
		Type   string `xml:"type,attr"`
		Length string `xml:"length,attr"`
	} `xml:"enclosure"`
	Duration    string   `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd duration"`
	ItunesImage xmlImage `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd image"`
	Season      string   `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd season"`
	Episode     string   `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd episode"`
	EpisodeType string   `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd episodeType"`
	Explicit    string   `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd explicit"`
}

type xmlRoot struct {
	XMLName xml.Name   `xml:"rss"`
	Channel xmlChannel `xml:"channel"`
}

// nsFix makes the iTunes and content namespaces match no matter how a feed
// spells them (wrong URI scheme, or a prefix that was never declared).
type nsFix struct{ d *xml.Decoder }

func (n nsFix) Token() (xml.Token, error) {
	t, err := n.d.Token()
	switch v := t.(type) {
	case xml.StartElement:
		v.Name.Space = fixSpace(v.Name.Space)
		return v, err
	case xml.EndElement:
		v.Name.Space = fixSpace(v.Name.Space)
		return v, err
	}
	return t, err
}

func fixSpace(s string) string {
	switch strings.ToLower(s) {
	case "itunes", "https://www.itunes.com/dtds/podcast-1.0.dtd", itunesNS:
		return itunesNS
	case "content", contentNS:
		return contentNS
	}
	return s
}

// ParseFeed parses RSS 2.0 (with the iTunes extensions) into a Feed.
func ParseFeed(data []byte) (*Feed, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.CharsetReader = charset.NewReaderLabel // ISO-8859-1, Windows-1252, ...
	dec.Strict = false

	var root xmlRoot
	if err := xml.NewTokenDecoder(nsFix{dec}).Decode(&root); err != nil {
		return nil, &FeedError{"this URL is not a valid RSS podcast feed"}
	}
	ch := root.Channel

	f := &Feed{
		Title:       cleanText(ch.Title, maxTitleRunes),
		Author:      cleanText(ch.Author, 200),
		Description: cleanText(firstNonEmpty(ch.Description, ch.Summary), maxShowDescRunes),
		ImageURL:    cleanURL(firstNonEmpty(ch.ItunesImage.Href, ch.Image.URL)),
		Link:        cleanURL(ch.Link),
		Language:    cleanText(ch.Language, 20),
		Explicit:    parseExplicit(ch.Explicit),
	}
	if f.Title == "" {
		return nil, &FeedError{"the feed has no title"}
	}

	seen := map[string]int{}
	for _, it := range ch.Items {
		audio := cleanURL(it.Enclosure.URL)
		if audio == "" {
			continue // not an audio episode (or a broken one)
		}
		ep := Episode{
			GUID:          strings.TrimSpace(it.GUID),
			Title:         cleanText(it.Title, maxTitleRunes),
			Description:   cleanText(firstNonEmpty(it.Content, it.Description, it.Summary), maxEpisodeDescRunes),
			AudioURL:      audio,
			MimeType:      cleanText(strings.ToLower(it.Enclosure.Type), 100),
			DurationMs:    parseDuration(it.Duration),
			PublishedAt:   parseDate(it.PubDate),
			EpisodeType:   parseEpisodeType(it.EpisodeType),
			ImageURL:      cleanURL(it.ItunesImage.Href),
			Explicit:      parseExplicit(it.Explicit),
			Season:        parsePositiveInt(it.Season),
			EpisodeNumber: parsePositiveInt(it.Episode),
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(it.Enclosure.Length), 10, 64); err == nil && n > 0 {
			ep.SizeBytes = n
		}
		if ep.GUID == "" || utf8.RuneCountInString(ep.GUID) > 500 {
			ep.GUID = audio // the enclosure URL is the next most stable identity
		}
		if ep.Title == "" {
			ep.Title = "Untitled episode"
		}
		if i, dup := seen[ep.GUID]; dup {
			f.Episodes[i] = ep // a repeated guid: the later item wins
			continue
		}
		seen[ep.GUID] = len(f.Episodes)
		f.Episodes = append(f.Episodes, ep)
	}

	// Newest first; undated episodes sort as the oldest so they never crowd out dated ones.
	sort.SliceStable(f.Episodes, func(i, j int) bool { return f.Episodes[i].PublishedAt.After(f.Episodes[j].PublishedAt) })
	if len(f.Episodes) > MaxEpisodesPerFeed {
		f.Episodes = f.Episodes[:MaxEpisodesPerFeed]
	}
	return f, nil
}

// NormalizeFeedURL validates a user-supplied feed URL.
func NormalizeFeedURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLLen {
		return "", &FeedError{"feedUrl must be a http(s) URL"}
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "", &FeedError{"feedUrl must be a http(s) URL without credentials"}
	}
	u.Fragment = ""
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

// cleanURL keeps only well-formed http(s) URLs.
func cleanURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxURLLen {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return ""
	}
	return s
}

var (
	reBreak = regexp.MustCompile(`(?i)<\s*(br\s*/?|/p|/div|/li|/h[1-6])\s*>`)
	reTag   = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpace = regexp.MustCompile(`[ \t\r\f\v]+`)
	reLines = regexp.MustCompile(`\n{3,}`)
)

// cleanText turns feed text (often HTML) into plain text of at most max runes.
func cleanText(s string, max int) string {
	s = reBreak.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= 0x20 && r != 0x7f && r != 0xfffd {
			return r
		}
		return -1
	}, s)
	s = reSpace.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	s = strings.TrimSpace(reLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
	if utf8.RuneCountInString(s) > max {
		r := []rune(s)
		s = strings.TrimSpace(string(r[:max-1])) + "…"
	}
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func parseExplicit(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "yes", "true", "explicit":
		return true
	}
	return false
}

func parseEpisodeType(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trailer":
		return "trailer"
	case "bonus":
		return "bonus"
	}
	return "full"
}

func parsePositiveInt(s string) *int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 || n > 100000 {
		return nil
	}
	return &n
}

// parseDuration reads itunes:duration: seconds ("3600", "3600.5") or
// [HH:]MM:SS. Anything else, or an absurd value, is 0 (unknown).
func parseDuration(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	total := 0.0
	for _, part := range strings.Split(s, ":") {
		v, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil || v < 0 {
			return 0
		}
		total = total*60 + v
	}
	if strings.Count(s, ":") > 2 {
		return 0
	}
	ms := int(total * 1000)
	if ms > maxDurationMs {
		return 0
	}
	return ms
}

var dateLayouts = []string{
	time.RFC1123Z, time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST",
	"Mon, 02 Jan 2006 15:04 -0700", "Mon, 2 Jan 2006 15:04 -0700",
	"2 Jan 2006 15:04:05 -0700", "2 Jan 2006 15:04:05 MST",
	"Mon, 2 Jan 2006 15:04:05", "Mon, 2 Jan 06 15:04:05 -0700",
	time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02",
}

func parseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
