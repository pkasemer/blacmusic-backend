package youtube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Google is the real Provider: Google OAuth plus the YouTube Data API v3. The
// base URLs are fields so tests can point it at a local fake server.
type Google struct {
	ClientID, ClientSecret, RedirectURL string
	AuthBase, TokenURL, RevokeURL       string
	APIBase                             string
	HTTP                                *http.Client
}

// NewGoogle returns a provider for the given OAuth client.
func NewGoogle(clientID, clientSecret, redirectURL string) *Google {
	return &Google{
		ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURL,
		AuthBase:  "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:  "https://oauth2.googleapis.com/token",
		RevokeURL: "https://oauth2.googleapis.com/revoke",
		APIBase:   "https://www.googleapis.com/youtube/v3",
		HTTP:      &http.Client{Timeout: 20 * time.Second},
	}
}

// AuthURL builds the consent URL: read-only scope, offline access (so we get a
// refresh token) and PKCE.
func (g *Google) AuthURL(state, challenge string) string {
	q := url.Values{
		"client_id":             {g.ClientID},
		"redirect_uri":          {g.RedirectURL},
		"response_type":         {"code"},
		"scope":                 {ReadOnlyScope},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return g.AuthBase + "?" + q.Encode()
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
}

func (g *Google) token(ctx context.Context, form url.Values) (*Token, error) {
	form.Set("client_id", g.ClientID)
	form.Set("client_secret", g.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	body, status, err := g.do(req)
	if err != nil {
		return nil, err
	}
	var tr tokenResponse
	_ = json.Unmarshal(body, &tr)
	if tr.Error == "invalid_grant" {
		return nil, ErrInvalidGrant
	}
	if status != http.StatusOK || tr.AccessToken == "" {
		return nil, &APIError{Status: status, Body: trim(body)}
	}
	return &Token{
		AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken, Scope: tr.Scope,
		Expiry: time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

// Exchange trades the authorisation code (with the PKCE verifier) for tokens.
func (g *Google) Exchange(ctx context.Context, code, verifier string) (*Token, error) {
	return g.token(ctx, url.Values{
		"code": {code}, "code_verifier": {verifier}, "grant_type": {"authorization_code"}, "redirect_uri": {g.RedirectURL},
	})
}

// Refresh gets a fresh access token.
func (g *Google) Refresh(ctx context.Context, refreshToken string) (*Token, error) {
	return g.token(ctx, url.Values{"refresh_token": {refreshToken}, "grant_type": {"refresh_token"}})
}

// Revoke invalidates a token at Google. An already invalid token is not an error.
func (g *Google) Revoke(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.RevokeURL, strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	body, status, err := g.do(req)
	if err != nil {
		return err
	}
	if status == http.StatusOK || status == http.StatusBadRequest { // 400 = already revoked or unknown
		return nil
	}
	return &APIError{Status: status, Body: trim(body)}
}

func (g *Google) do(req *http.Request) ([]byte, int, error) {
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return body, resp.StatusCode, err
}

// api GETs a Data API path and decodes the JSON into out.
func (g *Google) api(ctx context.Context, accessToken, path string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.APIBase+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	body, status, err := g.do(req)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		low := strings.ToLower(string(body))
		switch {
		case strings.Contains(low, "quotaexceeded") || strings.Contains(low, "dailylimitexceeded"):
			return ErrQuota
		case status == http.StatusUnauthorized:
			return ErrInvalidGrant
		}
		return &APIError{Status: status, Body: trim(body)}
	}
	return json.Unmarshal(body, out)
}

type thumbs map[string]struct {
	URL string `json:"url"`
}

func (t thumbs) best() string {
	for _, k := range []string{"maxres", "standard", "high", "medium", "default"} {
		if v, ok := t[k]; ok && v.URL != "" {
			return v.URL
		}
	}
	return ""
}

type channelsResponse struct {
	Items []struct {
		ID      string `json:"id"`
		Snippet struct {
			Title      string `json:"title"`
			Thumbnails thumbs `json:"thumbnails"`
		} `json:"snippet"`
		ContentDetails struct {
			RelatedPlaylists struct {
				Uploads string `json:"uploads"`
			} `json:"relatedPlaylists"`
		} `json:"contentDetails"`
	} `json:"items"`
}

func (g *Google) channel(ctx context.Context, accessToken string) (*Channel, string, error) {
	var r channelsResponse
	if err := g.api(ctx, accessToken, "/channels", url.Values{"part": {"snippet,contentDetails"}, "mine": {"true"}}, &r); err != nil {
		return nil, "", err
	}
	if len(r.Items) == 0 {
		return nil, "", ErrNoChannel
	}
	it := r.Items[0]
	return &Channel{ID: it.ID, Title: it.Snippet.Title, ThumbnailURL: it.Snippet.Thumbnails.best()}, it.ContentDetails.RelatedPlaylists.Uploads, nil
}

// Channel returns the connected account's channel.
func (g *Google) Channel(ctx context.Context, accessToken string) (*Channel, error) {
	c, _, err := g.channel(ctx, accessToken)
	return c, err
}

// Videos lists uploads through the channel's uploads playlist (1 quota unit per
// 50 videos; search.list would cost 100).
func (g *Google) Videos(ctx context.Context, accessToken string, max int) ([]Video, error) {
	_, playlist, err := g.channel(ctx, accessToken)
	if err != nil {
		return nil, err
	}
	var ids []string
	page := ""
	for len(ids) < max {
		q := url.Values{"part": {"contentDetails"}, "playlistId": {playlist}, "maxResults": {"50"}}
		if page != "" {
			q.Set("pageToken", page)
		}
		var r struct {
			NextPageToken string `json:"nextPageToken"`
			Items         []struct {
				ContentDetails struct {
					VideoID string `json:"videoId"`
				} `json:"contentDetails"`
			} `json:"items"`
		}
		if err := g.api(ctx, accessToken, "/playlistItems", q, &r); err != nil {
			return nil, err
		}
		for _, it := range r.Items {
			if it.ContentDetails.VideoID != "" && len(ids) < max {
				ids = append(ids, it.ContentDetails.VideoID)
			}
		}
		if r.NextPageToken == "" {
			break
		}
		page = r.NextPageToken
	}

	var out []Video
	for i := 0; i < len(ids); i += 50 {
		end := min(i+50, len(ids))
		var r struct {
			Items []struct {
				ID      string `json:"id"`
				Snippet struct {
					Title       string    `json:"title"`
					Description string    `json:"description"`
					PublishedAt time.Time `json:"publishedAt"`
					Thumbnails  thumbs    `json:"thumbnails"`
				} `json:"snippet"`
				ContentDetails struct {
					Duration string `json:"duration"`
				} `json:"contentDetails"`
			} `json:"items"`
		}
		q := url.Values{"part": {"snippet,contentDetails"}, "id": {strings.Join(ids[i:end], ",")}, "maxResults": {"50"}}
		if err := g.api(ctx, accessToken, "/videos", q, &r); err != nil {
			return nil, err
		}
		for _, it := range r.Items {
			out = append(out, Video{
				ID: it.ID, Title: it.Snippet.Title, Description: it.Snippet.Description,
				PublishedAt: it.Snippet.PublishedAt, DurationSec: ParseDuration(it.ContentDetails.Duration),
				ThumbnailURL: it.Snippet.Thumbnails.best(),
			})
		}
	}
	return out, nil
}

var isoDuration = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$`)

// ParseDuration reads an ISO 8601 duration such as PT3M12S into seconds (0 if unreadable).
func ParseDuration(s string) int {
	m := isoDuration.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	return n(1)*86400 + n(2)*3600 + n(3)*60 + n(4)
}

func trim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return fmt.Sprint(s)
}
