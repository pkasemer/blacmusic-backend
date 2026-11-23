package podcast

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const sampleFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel>
  <title>  The Test Show </title>
  <link>https://example.com/show</link>
  <description><![CDATA[<p>A show about <b>testing</b> &amp; things.</p><p>Second paragraph.</p>]]></description>
  <language>en-us</language>
  <itunes:author>Test Author</itunes:author>
  <itunes:image href="https://example.com/cover.jpg"/>
  <itunes:explicit>yes</itunes:explicit>
  <item>
    <title>Episode Two</title>
    <guid isPermaLink="false">ep-2</guid>
    <pubDate>Tue, 12 Mar 2024 08:30:00 +0000</pubDate>
    <description>short</description>
    <content:encoded><![CDATA[<p>Long <a href="x">notes</a></p><ul><li>one</li><li>two</li></ul>]]></content:encoded>
    <enclosure url="https://cdn.example.com/ep2.mp3" length="12345678" type="audio/mpeg"/>
    <itunes:duration>01:02:03</itunes:duration>
    <itunes:season>2</itunes:season>
    <itunes:episode>5</itunes:episode>
    <itunes:episodeType>bonus</itunes:episodeType>
    <itunes:image href="https://example.com/ep2.jpg"/>
  </item>
  <item>
    <title>Episode One</title>
    <guid>ep-1</guid>
    <pubDate>Mon, 4 Mar 2024 08:30:00 GMT</pubDate>
    <enclosure url="https://cdn.example.com/ep1.m4a" length="0" type="audio/x-m4a"/>
    <itunes:duration>754</itunes:duration>
  </item>
  <item>
    <title>An article, not audio</title>
    <guid>post-1</guid>
    <link>https://example.com/post</link>
  </item>
  <item>
    <title>No guid</title>
    <pubDate>Sun, 3 Mar 2024 08:30:00 GMT</pubDate>
    <enclosure url="https://cdn.example.com/noguid.mp3" type="audio/mpeg"/>
  </item>
</channel>
</rss>`

func TestParseFeed(t *testing.T) {
	f, err := ParseFeed([]byte(sampleFeed))
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "The Test Show" || f.Author != "Test Author" || f.Language != "en-us" || !f.Explicit {
		t.Fatalf("show metadata: %+v", f)
	}
	if f.ImageURL != "https://example.com/cover.jpg" || f.Link != "https://example.com/show" {
		t.Fatalf("show urls: %q %q", f.ImageURL, f.Link)
	}
	if want := "A show about testing & things.\nSecond paragraph."; f.Description != want {
		t.Fatalf("description = %q, want %q", f.Description, want)
	}

	if len(f.Episodes) != 3 {
		t.Fatalf("want 3 audio episodes (the article is skipped), got %d", len(f.Episodes))
	}
	// Newest first.
	if f.Episodes[0].GUID != "ep-2" || f.Episodes[1].GUID != "ep-1" {
		t.Fatalf("order: %s %s %s", f.Episodes[0].GUID, f.Episodes[1].GUID, f.Episodes[2].GUID)
	}

	e := f.Episodes[0]
	if e.AudioURL != "https://cdn.example.com/ep2.mp3" || e.MimeType != "audio/mpeg" || e.SizeBytes != 12345678 {
		t.Fatalf("enclosure: %+v", e)
	}
	if e.DurationMs != (1*3600+2*60+3)*1000 {
		t.Fatalf("duration = %d", e.DurationMs)
	}
	if !e.PublishedAt.Equal(time.Date(2024, 3, 12, 8, 30, 0, 0, time.UTC)) {
		t.Fatalf("published = %v", e.PublishedAt)
	}
	if e.Season == nil || *e.Season != 2 || e.EpisodeNumber == nil || *e.EpisodeNumber != 5 || e.EpisodeType != "bonus" {
		t.Fatalf("numbering: %+v", e)
	}
	if want := "Long notes\none\ntwo"; e.Description != want { // content:encoded preferred, HTML stripped
		t.Fatalf("episode description = %q, want %q", e.Description, want)
	}
	if e.ImageURL != "https://example.com/ep2.jpg" {
		t.Fatalf("episode image = %q", e.ImageURL)
	}
	if f.Episodes[1].DurationMs != 754000 || f.Episodes[1].EpisodeType != "full" {
		t.Fatalf("ep-1: %+v", f.Episodes[1])
	}
	// A missing guid falls back to the enclosure URL.
	if f.Episodes[2].GUID != "https://cdn.example.com/noguid.mp3" || f.Episodes[2].Title != "No guid" {
		t.Fatalf("no-guid episode: %+v", f.Episodes[2])
	}
}

func TestParseFeedLatin1AndUndeclaredPrefix(t *testing.T) {
	// ISO-8859-1 bytes (é = 0xE9) and an itunes: prefix that was never declared.
	raw := "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><rss version=\"2.0\"><channel><title>Caf\xe9 Show</title>" +
		"<itunes:author>Ren\xe9</itunes:author>" +
		"<item><title>Un \xe9pisode</title><guid>1</guid><enclosure url=\"http://example.com/a.mp3\" type=\"audio/mpeg\"/>" +
		"<itunes:duration>10:00</itunes:duration></item></channel></rss>"
	f, err := ParseFeed([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "Café Show" || f.Author != "René" || f.Episodes[0].Title != "Un épisode" {
		t.Fatalf("charset: %q %q %q", f.Title, f.Author, f.Episodes[0].Title)
	}
	if f.Episodes[0].DurationMs != 600000 {
		t.Fatalf("duration via undeclared prefix = %d", f.Episodes[0].DurationMs)
	}
}

func TestParseFeedRejectsNonFeeds(t *testing.T) {
	cases := map[string]string{
		"html page": `<!DOCTYPE html><html><head><title>Not a feed</title></head><body></body></html>`,
		"json":      `{"title":"nope"}`,
		"empty":     ``,
		"atom":      `<feed xmlns="http://www.w3.org/2005/Atom"><title>x</title></feed>`,
		"no title":  `<rss version="2.0"><channel><description>x</description></channel></rss>`,
		"truncated": `<rss version="2.0"><channel><title>x</title><item><title>`,
	}
	for name, body := range cases {
		_, err := ParseFeed([]byte(body))
		var fe *FeedError
		if !errors.As(err, &fe) {
			t.Errorf("%s: err = %v, want FeedError", name, err)
		}
	}
}

func TestParseFeedSanitisesURLsAndText(t *testing.T) {
	raw := `<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"><channel><title>T</title>
	<link>javascript:alert(1)</link>
	<itunes:image href="file:///etc/passwd"/>
	<item><title><![CDATA[<script>alert(1)</script>Hi]]></title><guid>1</guid>
	  <enclosure url="ftp://example.com/a.mp3" type="audio/mpeg"/></item>
	<item><title>Good</title><guid>2</guid><enclosure url="https://user:pw@example.com/a.mp3"/></item>
	<item><title>Good</title><guid>3</guid><enclosure url="https://example.com/ok.mp3"/>
	  <itunes:duration>99999999</itunes:duration></item>
	</channel></rss>`
	f, err := ParseFeed([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if f.Link != "" || f.ImageURL != "" {
		t.Fatalf("unsafe show URLs kept: %q %q", f.Link, f.ImageURL)
	}
	if len(f.Episodes) != 1 || f.Episodes[0].GUID != "3" {
		t.Fatalf("only the https episode without credentials should survive: %+v", f.Episodes)
	}
	if f.Episodes[0].DurationMs != 0 {
		t.Fatalf("absurd duration kept: %d", f.Episodes[0].DurationMs)
	}
}

func TestParseFeedKeepsNewestEpisodesWhenOverCap(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<rss version="2.0"><channel><title>Big</title>`)
	for i := 0; i < MaxEpisodesPerFeed+50; i++ {
		fmt.Fprintf(&b, `<item><title>e%d</title><guid>g%d</guid><pubDate>%s</pubDate><enclosure url="https://example.com/%d.mp3"/></item>`,
			i, i, time.Unix(1_600_000_000+int64(i)*3600, 0).UTC().Format(time.RFC1123Z), i)
	}
	b.WriteString(`</channel></rss>`)
	f, err := ParseFeed([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Episodes) != MaxEpisodesPerFeed {
		t.Fatalf("kept %d, want %d", len(f.Episodes), MaxEpisodesPerFeed)
	}
	if f.Episodes[0].GUID != fmt.Sprintf("g%d", MaxEpisodesPerFeed+49) {
		t.Fatalf("newest episode not first: %s", f.Episodes[0].GUID)
	}
}

func TestParseDuration(t *testing.T) {
	cases := map[string]int{
		"": 0, "0": 0, "90": 90000, "90.5": 90500, "1:30": 90000, "01:02:03": 3723000,
		"1:02:03.5": 3723500, "abc": 0, "1:2:3:4": 0, "-5": 0, "9999999": 0,
	}
	for in, want := range cases {
		if got := parseDuration(in); got != want {
			t.Errorf("parseDuration(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseDate(t *testing.T) {
	want := time.Date(2024, 3, 12, 8, 30, 0, 0, time.UTC)
	for _, in := range []string{
		"Tue, 12 Mar 2024 08:30:00 +0000", "Tue, 12 Mar 2024 08:30:00 GMT", "12 Mar 2024 08:30:00 +0000",
		"2024-03-12T08:30:00Z", "Tue, 12 Mar 2024 10:30:00 +0200", "Tue, 12 Mar 2024 08:30 +0000",
	} {
		if got := parseDate(in); !got.Equal(want) {
			t.Errorf("parseDate(%q) = %v, want %v", in, got, want)
		}
	}
	if !parseDate("not a date").IsZero() || !parseDate("").IsZero() {
		t.Error("bad dates must parse to the zero time")
	}
}

func TestNormalizeFeedURL(t *testing.T) {
	ok := map[string]string{
		"https://Example.com/feed.xml":    "https://example.com/feed.xml",
		"  http://example.com/rss#frag  ": "http://example.com/rss",
		"https://example.com:8443/a?b=c":  "https://example.com:8443/a?b=c",
	}
	for in, want := range ok {
		got, err := NormalizeFeedURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeFeedURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "ftp://example.com/x", "file:///etc/passwd", "javascript:alert(1)", "example.com/feed",
		"https://user:pw@example.com/feed", "https://", strings.Repeat("a", 3000)} {
		if _, err := NormalizeFeedURL(in); err == nil {
			t.Errorf("NormalizeFeedURL(%q) must fail", in)
		}
	}
}

func TestCleanText(t *testing.T) {
	cases := map[string]string{
		"<p>Hello &amp; welcome</p><p>Bye</p>": "Hello & welcome\nBye",
		"a<br/>b<br>c":                         "a\nb\nc",
		"  lots   of    space  ":               "lots of space",
		"<script>x()</script>y":                "x()y", // tags are removed; the result is plain text, never markup
		"\x00bad\x07chars":                     "badchars",
	}
	for in, want := range cases {
		if got := cleanText(in, 1000); got != want {
			t.Errorf("cleanText(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("é", 50)
	if got := cleanText(long, 10); len([]rune(got)) != 10 || !strings.HasSuffix(got, "…") {
		t.Errorf("truncation: %q", got)
	}
}
