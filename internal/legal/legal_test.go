package legal

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, h *Handler, page string) (int, string, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/"+page, nil)
	if page == "privacy" {
		h.Privacy(rec, req)
	} else {
		h.Terms(rec, req)
	}
	hd := map[string]string{}
	for k := range rec.Header() {
		hd[k] = rec.Header().Get(k)
	}
	return rec.Code, rec.Body.String(), hd
}

func TestPagesRender(t *testing.T) {
	h, err := New("Blac Music", "https://blacmusic.pkasemer.com", "legal@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	code, body, hd := get(t, h, "privacy")
	if code != 200 || !strings.HasPrefix(hd["Content-Type"], "text/html") || hd["Cache-Control"] != "public, max-age=3600" {
		t.Fatalf("privacy = %d %v", code, hd)
	}
	for _, want := range []string{
		"<title>Privacy Policy - Blac Music</title>", "Last updated " + Updated, "mailto:legal@example.com",
		// what Google requires for the YouTube permission
		"Google API Services User Data Policy", "Limited Use requirements", "youtube.readonly", "myaccount.google.com/permissions",
		"We do not sell", "Delete", "href=\"/terms\"",
	} {
		if want == "Delete" {
			want = "delete your account"
		}
		if !strings.Contains(body, want) {
			t.Errorf("privacy lacks %q", want)
		}
	}
	code, body, _ = get(t, h, "terms")
	if code != 200 {
		t.Fatalf("terms = %d", code)
	}
	for _, want := range []string{"<title>Terms of Service - Blac Music</title>", "Copyright complaints", "youtube.com/t/terms", "mailto:legal@example.com", "laws of Uganda", `href="/privacy"`} {
		if !strings.Contains(body, want) {
			t.Errorf("terms lacks %q", want)
		}
	}
	if strings.Contains(body, "{{") || strings.Contains(body, "<no value>") {
		t.Fatal("an unfilled placeholder is left in the page")
	}
}

func TestWithoutContactEmail(t *testing.T) {
	h, err := New("Blac Music", "https://x.example", "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []string{"privacy", "terms"} {
		_, body, _ := get(t, h, page)
		if strings.Contains(body, "mailto:") || strings.Contains(body, "Email:") {
			t.Errorf("%s shows an empty contact address", page)
		}
	}
}

func TestOperatorNameIsEscaped(t *testing.T) {
	h, err := New(`<script>alert(1)</script>`, "https://x.example", `"><img src=x onerror=alert(1)>`, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []string{"privacy", "terms"} {
		_, body, _ := get(t, h, page)
		if strings.Contains(body, "<script>alert(1)") || strings.Contains(body, "<img src=x") {
			t.Errorf("%s does not escape the configured values", page)
		}
	}
}

func TestHomePage(t *testing.T) {
	h, err := New("Blac Music", "https://blacmusic.pkasemer.com", "legal@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.Home(rec, httptest.NewRequest("GET", "/", nil))
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("home = %d", rec.Code)
	}
	for _, want := range []string{"<title>Blac Music</title>", `href="/privacy"`, `href="/terms"`, "read-only", "do not sell", "mailto:legal@example.com"} {
		if !strings.Contains(body, want) {
			t.Errorf("home lacks %q", want)
		}
	}
	// only the root: anything else under "/" stays a 404
	rec = httptest.NewRecorder()
	h.Home(rec, httptest.NewRequest("GET", "/nope", nil))
	if rec.Code != 404 {
		t.Fatalf("/nope = %d", rec.Code)
	}
}

func TestTextsFollowTheAudioImportSetting(t *testing.T) {
	off, err := New("Blac Music", "https://x.example", "a@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	on, err := New("Blac Music", "https://x.example", "a@example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	_, offPrivacy, _ := get(t, off, "privacy")
	_, onPrivacy, _ := get(t, on, "privacy")
	if !strings.Contains(offPrivacy, "we do not download, copy or store your videos") || strings.Contains(offPrivacy, "Importing the audio") {
		t.Error("with the import off, the policy must say audio is never fetched")
	}
	if strings.Contains(onPrivacy, "we do not download") || !strings.Contains(onPrivacy, "fetch the audio-only stream") || !strings.Contains(onPrivacy, "own the rights") {
		t.Error("with the import on, the policy must describe it and must not deny it")
	}
	_, offTerms, _ := get(t, off, "terms")
	_, onTerms, _ := get(t, on, "terms")
	if !strings.Contains(offTerms, "You must upload the audio yourself") || strings.Contains(onTerms, "You must upload the audio yourself") || !strings.Contains(onTerms, "allowed to copy it from YouTube") {
		t.Error("the terms do not follow the setting")
	}
	for name, h := range map[string]*Handler{"off": off, "on": on} {
		rec := httptest.NewRecorder()
		h.Home(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "{{") {
			t.Errorf("home (%s) = %d", name, rec.Code)
		}
	}
}
