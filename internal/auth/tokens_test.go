package auth

import (
	"testing"
	"time"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func TestAccessTokenRoundTrip(t *testing.T) {
	ts := NewTokenService(testSecret, time.Minute)
	tok, _, err := ts.NewAccess("user-1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ts.ParseAccess(tok)
	if err != nil || got != "user-1" {
		t.Fatalf("ParseAccess = %q, %v", got, err)
	}
}

func TestAccessTokenRejectsWrongSecret(t *testing.T) {
	tok, _, _ := NewTokenService(testSecret, time.Minute).NewAccess("user-1")
	other := NewTokenService("ffffffffffffffffffffffffffffffff", time.Minute)
	if _, err := other.ParseAccess(tok); err == nil {
		t.Fatal("expected failure with a different secret")
	}
}

func TestAccessTokenRejectsExpired(t *testing.T) {
	ts := NewTokenService(testSecret, -time.Minute)
	tok, _, _ := ts.NewAccess("user-1")
	if _, err := ts.ParseAccess(tok); err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestAccessTokenRejectsGarbage(t *testing.T) {
	ts := NewTokenService(testSecret, time.Minute)
	if _, err := ts.ParseAccess("not.a.jwt"); err == nil {
		t.Fatal("expected garbage to be rejected")
	}
}

func TestRefreshTokenHashStable(t *testing.T) {
	raw, hash, err := NewRefresh()
	if err != nil {
		t.Fatal(err)
	}
	if HashRefresh(raw) != hash {
		t.Fatal("hash must be deterministic")
	}
	raw2, _, _ := NewRefresh()
	if raw == raw2 {
		t.Fatal("refresh tokens must be unique")
	}
}

func TestPassword(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "correct horse battery") {
		t.Fatal("correct password rejected")
	}
	if CheckPassword(h, "wrong") {
		t.Fatal("wrong password accepted")
	}
}
