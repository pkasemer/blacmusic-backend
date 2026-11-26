package youtube

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Fake is an in-process Provider with canned data. It serves development
// (YOUTUBE_FAKE=true, so the connect flow works without a Google project) and
// tests. Its consent "page" is the callback URL itself: opening AuthURL
// connects at once.
type Fake struct {
	CallbackURL string
	Chan        Channel
	List        []Video

	mu        sync.Mutex
	Revoked   []string
	RevokeErr error
	// RefreshErr, when set, is returned by Refresh (for example ErrInvalidGrant).
	RefreshErr error
	// VideosErr, when set, is returned by Videos.
	VideosErr error
	Calls     map[string]int
}

func (f *Fake) count(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Calls == nil {
		f.Calls = map[string]int{}
	}
	f.Calls[name]++
}

// NewFake returns a fake channel "Akeine (demo)" whose uploads show the usual
// mess: one song with an audio upload, a lyric video, a short and a promo.
func NewFake(callbackURL string) *Fake {
	now := time.Now().UTC()
	d := func(days int) time.Time { return now.AddDate(0, 0, -days) }
	return &Fake{
		CallbackURL: callbackURL,
		Chan:        Channel{ID: "UCfake0000000000000000", Title: "Akeine (demo channel)", ThumbnailURL: "https://example.invalid/avatar.jpg"},
		List: []Video{
			{ID: "demoMukeneAud", Title: "MUKENE (Official Audio)", PublishedAt: d(500), DurationSec: 173},
			{ID: "demoMukeneLyr", Title: "Mukene lyric video", PublishedAt: d(480), DurationSec: 173},
			{ID: "demoMukeneSht", Title: "MUKENE lyrics short #shorts", PublishedAt: d(470), DurationSec: 30},
			{ID: "demoBlessedLyr", Title: "BLESSED LYRICS VIDEO", PublishedAt: d(90), DurationSec: 180},
			{ID: "demoBetterVis", Title: "BETTER VISUALIZER (ft Wana Benjamin)", PublishedAt: d(300), DurationSec: 206},
			{ID: "demoWantedAud", Title: "WANTED full audio", PublishedAt: d(2200), DurationSec: 153},
			{ID: "demoPromo", Title: "BLESSED OUT NOW ON ALL STREAMING PLATFORMS!!!", PublishedAt: d(88), DurationSec: 95},
		},
	}
}

func (f *Fake) AuthURL(state, _ string) string {
	return f.CallbackURL + "?" + url.Values{"state": {state}, "code": {"fake-code"}}.Encode()
}

func (f *Fake) Exchange(_ context.Context, code, _ string) (*Token, error) {
	f.count("exchange")
	if !strings.HasPrefix(code, "fake") {
		return nil, ErrInvalidGrant
	}
	return &Token{AccessToken: "fake-access", RefreshToken: "fake-refresh", Expiry: time.Now().Add(time.Hour), Scope: ReadOnlyScope}, nil
}

func (f *Fake) Refresh(_ context.Context, refreshToken string) (*Token, error) {
	f.count("refresh")
	if f.RefreshErr != nil {
		return nil, f.RefreshErr
	}
	if refreshToken != "fake-refresh" {
		return nil, ErrInvalidGrant
	}
	return &Token{AccessToken: "fake-access", Expiry: time.Now().Add(time.Hour), Scope: ReadOnlyScope}, nil
}

func (f *Fake) Channel(_ context.Context, _ string) (*Channel, error) {
	f.count("channel")
	c := f.Chan
	return &c, nil
}

func (f *Fake) Videos(_ context.Context, _ string, max int) ([]Video, error) {
	f.count("videos")
	if f.VideosErr != nil {
		return nil, f.VideosErr
	}
	out := append([]Video(nil), f.List...)
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

func (f *Fake) Revoke(_ context.Context, token string) error {
	f.count("revoke")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Revoked = append(f.Revoked, token)
	return f.RevokeErr
}
