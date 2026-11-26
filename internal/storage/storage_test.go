package storage

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/blacmusic/backend/internal/config"
)

func newStore(t *testing.T, publicEndpoint string, ssl bool) *Store {
	t.Helper()
	s, err := New(config.Config{
		S3Endpoint: "storage:9000", S3PublicEndpoint: publicEndpoint, S3PublicUseSSL: ssl,
		S3Region: "us-east-1", S3AccessKey: "key", S3SecretKey: "secret", S3Bucket: "blac-media",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func host(t *testing.T, raw string, err error) (string, string) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	u, perr := url.Parse(raw)
	if perr != nil {
		t.Fatal(perr)
	}
	return u.Scheme, u.Host
}

func TestPresignedLinksUseTheConfiguredPublicHost(t *testing.T) {
	s := newStore(t, "localhost:9000", false)
	raw, err := s.PresignGet(context.Background(), "a/b.m4a", time.Hour)
	scheme, h := host(t, raw, err)
	if scheme != "http" || h != "localhost:9000" {
		t.Fatalf("got %s://%s, want http://localhost:9000", scheme, h)
	}
}

func TestPresignedLinksCanNameTheHostTheClientUsed(t *testing.T) {
	s := newStore(t, "localhost:9000", false)
	ctx := WithPublicHost(context.Background(), "192.168.1.4")

	// Every kind of link follows the request host, and keeps the configured port.
	get, err := s.PresignGet(ctx, "a.m4a", time.Hour)
	if _, h := host(t, get, err); h != "192.168.1.4:9000" {
		t.Fatalf("GET host = %s", h)
	}
	put, err := s.PresignPut(ctx, "b", time.Hour)
	if _, h := host(t, put, err); h != "192.168.1.4:9000" {
		t.Fatalf("PUT host = %s", h)
	}
	part, err := s.PresignPart(ctx, "c", "upload-1", 2, time.Hour)
	if _, h := host(t, part, err); h != "192.168.1.4:9000" {
		t.Fatalf("part host = %s", h)
	}
	pu, _ := url.Parse(part)
	if pu.Query().Get("partNumber") != "2" || pu.Query().Get("uploadId") != "upload-1" {
		t.Fatalf("part URL lost its query: %s", part)
	}

	// The signature belongs to that host: the same object signed for another host differs.
	other, _ := s.PresignGet(WithPublicHost(context.Background(), "10.0.2.2"), "a.m4a", time.Hour)
	if get == other {
		t.Fatal("links for different hosts must differ (the host is part of the signature)")
	}
	// A context without a host still uses the configured one.
	plain, err := s.PresignGet(context.Background(), "a.m4a", time.Hour)
	if _, h := host(t, plain, err); h != "localhost:9000" {
		t.Fatalf("plain host = %s", h)
	}
}

func TestOddHostsAreIgnoredAndSchemeFollowsConfig(t *testing.T) {
	s := newStore(t, "media.example.com", true) // no port configured, TLS
	for _, bad := range []string{"", "evil.com/path", "a b", "x@y", "host:9000", "-start", "a..b?x"} {
		raw, err := s.PresignGet(WithPublicHost(context.Background(), bad), "k", time.Hour)
		if _, h := host(t, raw, err); h != "media.example.com" {
			t.Errorf("host %q was honoured: %s", bad, h)
		}
	}
	raw, err := s.PresignGet(WithPublicHost(context.Background(), "lan.example"), "k", time.Hour)
	if scheme, h := host(t, raw, err); scheme != "https" || h != "lan.example" {
		t.Fatalf("got %s://%s, want https://lan.example (no port was configured)", scheme, h)
	}
}
