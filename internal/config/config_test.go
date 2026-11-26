package config

import "testing"

func TestLoadDevDefaults(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("JWT_SECRET", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q", c.HTTPAddr)
	}
	if c.S3PublicEndpoint != c.S3Endpoint {
		t.Errorf("public endpoint should default to internal endpoint")
	}
}

func TestProductionRequiresSecret(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("JWT_SECRET", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when JWT_SECRET is missing in production")
	}
}

func TestProductionRejectsDevSecret(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("JWT_SECRET", devJWTSecret)
	if _, err := Load(); err == nil {
		t.Fatal("expected error when dev secret used in production")
	}
}

func TestBadDuration(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("ACCESS_TOKEN_TTL", "banana")
	if _, err := Load(); err == nil {
		t.Fatal("expected duration parse error")
	}
}

func TestTrustProxyIsOffByDefault(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	c, err := Load()
	if err != nil || c.TrustProxy {
		t.Fatalf("TrustProxy = %v (err %v), want false unless asked for", c.TrustProxy, err)
	}
	t.Setenv("TRUST_PROXY", "true")
	if c, err = Load(); err != nil || !c.TrustProxy {
		t.Fatalf("TRUST_PROXY=true: %v %v", c.TrustProxy, err)
	}
	t.Setenv("TRUST_PROXY", "maybe")
	if _, err = Load(); err == nil {
		t.Fatal("a bad TRUST_PROXY value must be an error")
	}
}

func TestProductionNeedsSMTPAndRefusesPrivateFeeds(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	if _, err := Load(); err == nil {
		t.Fatal("production without SMTP_HOST must fail: verification emails could not be sent")
	}
	t.Setenv("SMTP_HOST", "smtp.example.com")
	if _, err := Load(); err != nil {
		t.Fatalf("production with SMTP: %v", err)
	}
	t.Setenv("FEED_ALLOW_PRIVATE", "true")
	if _, err := Load(); err == nil {
		t.Fatal("FEED_ALLOW_PRIVATE must be refused in production")
	}
}

func TestPublicHostFromRequestIsDevelopmentOnly(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("S3_PUBLIC_HOST_FROM_REQUEST", "true")
	if c, err := Load(); err != nil || !c.S3PublicHostFromRequest {
		t.Fatalf("development: %v %v", c.S3PublicHostFromRequest, err)
	}
	t.Setenv("APP_ENV", "production")
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("SMTP_HOST", "smtp.example.com")
	if _, err := Load(); err == nil {
		t.Fatal("S3_PUBLIC_HOST_FROM_REQUEST must be refused in production: clients could pick the signing host")
	}
}

func TestYouTubeSettings(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("JWT_SECRET", "")
	c, err := Load()
	if err != nil || c.GoogleClientID != "" || c.YouTubeFake || c.TransferConcurrency != 1 ||
		c.YouTubeAppRedirect != "blacmusic://youtube/connected" || c.YouTubeRedirectURL != "http://localhost:8080/v1/integrations/youtube/callback" {
		t.Fatalf("defaults = %+v %v", c, err)
	}

	t.Setenv("YOUTUBE_FAKE", "true")
	if c, err = Load(); err != nil || !c.YouTubeFake {
		t.Fatalf("fake in development = %v", err)
	}
	t.Setenv("TRANSFER_CONCURRENCY", "0")
	if _, err = Load(); err == nil {
		t.Fatal("concurrency 0 accepted")
	}
	t.Setenv("TRANSFER_CONCURRENCY", "2")
	t.Setenv("TOKEN_ENC_KEY", "abcd")
	if _, err = Load(); err == nil {
		t.Fatal("a short TOKEN_ENC_KEY was accepted")
	}
	t.Setenv("TOKEN_ENC_KEY", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if c, err = Load(); err != nil || len(c.TokenEncKey) != 32 || c.TransferConcurrency != 2 {
		t.Fatalf("valid key = %v", err)
	}
}

func TestYouTubeInProduction(t *testing.T) {
	prod := func() {
		t.Setenv("APP_ENV", "production")
		t.Setenv("JWT_SECRET", "a-production-secret-that-is-long-enough-123456")
		t.Setenv("SMTP_HOST", "smtp.example.com")
	}
	prod()
	t.Setenv("YOUTUBE_FAKE", "true")
	if _, err := Load(); err == nil {
		t.Fatal("the fake YouTube must not run in production")
	}
	t.Setenv("YOUTUBE_FAKE", "")
	t.Setenv("GOOGLE_CLIENT_ID", "id")
	if _, err := Load(); err == nil {
		t.Fatal("a client id without a secret was accepted")
	}
	t.Setenv("GOOGLE_CLIENT_SECRET", "secret")
	if _, err := Load(); err == nil {
		t.Fatal("production without TOKEN_ENC_KEY was accepted although YouTube connect is on")
	}
	t.Setenv("TOKEN_ENC_KEY", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if c, err := Load(); err != nil || c.GoogleClientID != "id" {
		t.Fatalf("complete production setup = %v", err)
	}
}

func TestEmailProviders(t *testing.T) {
	prod := func() {
		t.Setenv("APP_ENV", "production")
		t.Setenv("JWT_SECRET", "a-production-secret-that-is-long-enough-123456")
		t.Setenv("SMTP_HOST", "")
		t.Setenv("PLUNK_API_KEY", "")
	}
	prod()
	if _, err := Load(); err == nil {
		t.Fatal("production without any email provider was accepted")
	}
	prod()
	t.Setenv("PLUNK_API_KEY", "sk_test")
	c, err := Load()
	if err != nil || c.PlunkAPIKey != "sk_test" || c.MailFrom != c.SMTPFrom {
		t.Fatalf("plunk only = %+v %v", c, err)
	}
	t.Setenv("MAIL_FROM", "Blac Music <no-reply@pkasemer.com>")
	t.Setenv("PLUNK_API_URL", "https://plunk.example")
	if c, err = Load(); err != nil || c.MailFrom != "Blac Music <no-reply@pkasemer.com>" || c.PlunkURL != "https://plunk.example" {
		t.Fatalf("overrides = %+v %v", c, err)
	}
	prod()
	t.Setenv("SMTP_HOST", "smtp.example.com")
	if _, err := Load(); err != nil {
		t.Fatalf("smtp only: %v", err)
	}
}
