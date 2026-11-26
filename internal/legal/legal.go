// Package legal serves the public home page and the Privacy Policy and Terms of
// Service pages (GET /, GET /privacy and GET /terms). Google requires these on our own domain for
// sign-in and YouTube access, and the app stores link to them.
package legal

import (
	"bytes"
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"
)

// Updated is the date shown on both pages. Change it whenever the wording changes.
const Updated = "24 November 2026"

//go:embed privacy.html
var privacyHTML string

//go:embed terms.html
var termsHTML string

//go:embed home.html
var homeHTML string

const layout = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{if eq .Title "Blac Music"}}Blac Music{{else}}{{.Title}} - Blac Music{{end}}</title>
<meta name="robots" content="index,follow">
<style>
:root{--bg:#fff;--fg:#1d1d1f;--muted:#6b6b72;--line:#e4e4e8;--link:#6a3df0}
@media (prefers-color-scheme:dark){:root{--bg:#0f0f12;--fg:#ececf1;--muted:#9a9aa6;--line:#2a2a31;--link:#a58bff}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.65 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}
header,main,footer{max-width:720px;margin:0 auto;padding:0 20px}
header{padding-top:28px;padding-bottom:12px;border-bottom:1px solid var(--line);display:flex;justify-content:space-between;align-items:center;gap:12px;flex-wrap:wrap}
header a.brand{font-weight:700;color:var(--fg);text-decoration:none;font-size:18px}
nav a{margin-left:16px;color:var(--muted);text-decoration:none;font-size:14px}
nav a:hover{color:var(--link)}
h1{font-size:30px;line-height:1.2;margin:28px 0 4px}
h2{font-size:19px;margin:30px 0 8px}
.meta{color:var(--muted);margin:0 0 20px;font-size:14px}
a{color:var(--link)}
ul{padding-left:22px}
li{margin:6px 0}
code{background:var(--line);padding:1px 5px;border-radius:4px;font-size:.9em}
footer{padding-top:24px;padding-bottom:40px;color:var(--muted);font-size:14px;border-top:1px solid var(--line);margin-top:36px}
</style>
</head>
<body>
<header><a class="brand" href="{{.SiteURL}}">Blac Music</a><nav><a href="/privacy">Privacy</a><a href="/terms">Terms</a></nav></header>
<main>{{.Body}}</main>
<footer>&copy; {{.Operator}}</footer>
</body>
</html>`

// Handler serves the two pages.
type Handler struct {
	home, privacy, terms []byte
}

// New renders both pages once. operator is the name that runs the service,
// siteURL the public base URL and email the contact address (may be empty). audioFetch says
// whether the server may fetch the audio of an artist's own YouTube videos: the texts describe
// exactly what the service does, so they change with it.
func New(operator, siteURL, email string, audioFetch bool) (*Handler, error) {
	h := &Handler{}
	var err error
	if h.home, err = render("Blac Music", homeHTML, operator, siteURL, email, audioFetch); err != nil {
		return nil, err
	}
	if h.privacy, err = render("Privacy Policy", privacyHTML, operator, siteURL, email, audioFetch); err != nil {
		return nil, err
	}
	if h.terms, err = render("Terms of Service", termsHTML, operator, siteURL, email, audioFetch); err != nil {
		return nil, err
	}
	return h, nil
}

func render(title, body, operator, siteURL, email string, audioFetch bool) ([]byte, error) {
	data := map[string]any{"Operator": operator, "SiteURL": siteURL, "Email": email, "Updated": Updated, "AudioFetch": audioFetch}
	bt, err := template.New("body").Parse(body)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := bt.Execute(&b, data); err != nil {
		return nil, err
	}
	lt, err := template.New("layout").Parse(layout)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	err = lt.Execute(&out, map[string]any{
		"Title": title, "Body": template.HTML(b.String()), "SiteURL": siteURL, "Operator": operator, // the body was already escaped by html/template above
	})
	return out.Bytes(), err
}

func serve(page []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if _, err := w.Write(page); err != nil {
			slog.Debug("legal page write", "err", err)
		}
	}
}

// Home serves GET / : a short public page that says what Blac Music is. Google asks for
// a home page that describes the app and links to the privacy policy.
func (h *Handler) Home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	serve(h.home)(w, r)
}

// Privacy serves GET /privacy.
func (h *Handler) Privacy(w http.ResponseWriter, r *http.Request) { serve(h.privacy)(w, r) }

// Terms serves GET /terms.
func (h *Handler) Terms(w http.ResponseWriter, r *http.Request) { serve(h.terms)(w, r) }
