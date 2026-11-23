package podcast

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1", "10.0.0.5", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "169.254.0.1", "fe80::1", "fc00::1", "fd12:3456::1", "0.0.0.0", "::",
		"100.64.0.1", "100.127.255.255", "224.0.0.1", "ff02::1", "240.0.0.1", "198.18.0.1",
		"::ffff:127.0.0.1", "::ffff:10.1.2.3", "64:ff9b::7f00:1", "2002:7f00:1::1",
	}
	for _, s := range blocked {
		if ip := net.ParseIP(s); ip == nil || !blockedIP(ip) {
			t.Errorf("%s must be blocked", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "172.32.0.1", "100.128.0.1"}
	for _, s := range allowed {
		if ip := net.ParseIP(s); ip == nil || blockedIP(ip) {
			t.Errorf("%s must be allowed", s)
		}
	}
}

func TestFetchRefusesPrivateAddressesByDefault(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hit = true }))
	defer srv.Close() // listens on 127.0.0.1

	f := NewFetcher(false, 5*time.Second)
	_, err := f.Fetch(context.Background(), srv.URL, "", "")
	var fe *FeedError
	if !errors.As(err, &fe) || !strings.Contains(fe.Msg, "cannot be fetched") {
		t.Fatalf("err = %v, want a FeedError saying the address cannot be fetched", err)
	}
	if hit {
		t.Fatal("the request must never reach a loopback server")
	}
}

func TestFetchRefusesLoopbackLiterals(t *testing.T) {
	f := NewFetcher(false, 5*time.Second)
	for _, u := range []string{"http://127.0.0.1:1/x", "http://[::1]:1/x", "http://169.254.169.254/latest/meta-data/", "http://10.0.0.1/x", "http://localhost:1/x"} {
		_, err := f.Fetch(context.Background(), u, "", "")
		var fe *FeedError
		if !errors.As(err, &fe) {
			t.Errorf("%s: err = %v, want FeedError", u, err)
		}
	}
}

func TestFetchConditionalAndLimits(t *testing.T) {
	var gotINM, gotIMS string
	mux := http.NewServeMux()
	mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
		gotINM, gotIMS = r.Header.Get("If-None-Match"), r.Header.Get("If-Modified-Since")
		if gotINM == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Mon, 04 Mar 2024 08:30:00 GMT")
		_, _ = w.Write([]byte("<rss/>"))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		chunk := []byte(strings.Repeat("x", 1<<20))
		for i := 0; i < 12; i++ {
			_, _ = w.Write(chunk)
		}
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/feed", http.StatusFound) })
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := NewFetcher(true, 5*time.Second) // loopback allowed for this test
	ctx := context.Background()

	res, err := f.Fetch(ctx, srv.URL+"/feed", "", "")
	if err != nil || res.NotModified || string(res.Body) != "<rss/>" || res.ETag != `"v1"` || res.LastModified == "" {
		t.Fatalf("first fetch: %+v %v", res, err)
	}
	res, err = f.Fetch(ctx, srv.URL+"/feed", res.ETag, res.LastModified)
	if err != nil || !res.NotModified || gotINM != `"v1"` || gotIMS == "" {
		t.Fatalf("conditional fetch: %+v %v (INM=%q IMS=%q)", res, err, gotINM, gotIMS)
	}
	if res, err = f.Fetch(ctx, srv.URL+"/redirect", "", ""); err != nil || string(res.Body) != "<rss/>" {
		t.Fatalf("redirect: %+v %v", res, err)
	}

	for path, want := range map[string]string{"/big": "too large", "/missing": "HTTP 404", "/loop": "could not be reached"} {
		_, err := f.Fetch(ctx, srv.URL+path, "", "")
		var fe *FeedError
		if !errors.As(err, &fe) || !strings.Contains(fe.Msg, want) {
			t.Errorf("%s: err = %v, want FeedError containing %q", path, err, want)
		}
	}
}

func TestFetchTimeoutIsNotAFeedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	f := NewFetcher(true, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := f.Fetch(ctx, srv.URL, "", "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context deadline (so a cancelled refresh is not blamed on the feed)", err)
	}
}
