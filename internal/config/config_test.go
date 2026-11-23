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
