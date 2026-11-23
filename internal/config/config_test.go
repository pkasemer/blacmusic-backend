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
