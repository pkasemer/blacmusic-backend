package mailer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type captured struct {
	method, path, auth, ctype string
	body                      map[string]any
}

func plunkServer(t *testing.T, status int, answer string) (*Plunk, *captured) {
	t.Helper()
	c := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.method, c.path, c.auth, c.ctype = r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&c.body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	p, err := NewPlunk(srv.URL+"/", "sk_secret", "Blac Music <no-reply@pkasemer.com>")
	if err != nil {
		t.Fatal(err)
	}
	return p, c
}

func TestPlunkSendsTheRightRequest(t *testing.T) {
	p, c := plunkServer(t, 200, `{"success":true,"data":{"emails":[{"status":"queued"}]}}`)
	err := p.Send(context.Background(), "Kato <kato@example.com>", "Your Blac Music verification code", "Your code is 123456.\nIt works for 30 minutes.\n\nIgnore this if it was not you.")
	if err != nil {
		t.Fatal(err)
	}
	if c.method != "POST" || c.path != "/v1/send" || c.auth != "Bearer sk_secret" || c.ctype != "application/json" {
		t.Fatalf("request = %+v", c)
	}
	if c.body["to"] != "kato@example.com" || c.body["subject"] != "Your Blac Music verification code" || c.body["subscribed"] != false {
		t.Fatalf("body = %v", c.body)
	}
	from, _ := c.body["from"].(map[string]any)
	if from["email"] != "no-reply@pkasemer.com" || from["name"] != "Blac Music" {
		t.Fatalf("from = %v", c.body["from"])
	}
	html, _ := c.body["body"].(string)
	if !strings.Contains(html, "Your code is 123456.<br>It works for 30 minutes.") || !strings.Contains(html, "</p><p>Ignore this") {
		t.Fatalf("html = %s", html)
	}
}

func TestPlunkEscapesText(t *testing.T) {
	p, c := plunkServer(t, 200, `{"success":true}`)
	if err := p.Send(context.Background(), "a@example.com", "s", `<script>alert(1)</script> & "quotes"`); err != nil {
		t.Fatal(err)
	}
	html, _ := c.body["body"].(string)
	if strings.Contains(html, "<script>") || !strings.Contains(html, "&lt;script&gt;") || !strings.Contains(html, "&amp;") {
		t.Fatalf("html = %s", html)
	}
}

func TestPlunkRefusals(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		code   string
	}{
		{401, `{"success":false,"error":{"code":"INVALID_API_KEY","message":"Invalid API key","statusCode":401}}`, "INVALID_API_KEY"},
		{422, `{"success":false,"error":{"code":"VALIDATION_ERROR","message":"from domain is not verified"}}`, "VALIDATION_ERROR"},
		{429, `{"success":false,"error":{"code":"RATE_LIMITED","message":"slow down"}}`, "RATE_LIMITED"},
		{500, `oops`, ""},
	} {
		p, _ := plunkServer(t, tc.status, tc.body)
		err := p.Send(context.Background(), "a@example.com", "s", "b")
		var pe *Error
		if !errors.As(err, &pe) || pe.Status != tc.status || pe.Code != tc.code {
			t.Errorf("status %d: err = %v", tc.status, err)
		}
		if err != nil && strings.Contains(err.Error(), "sk_secret") {
			t.Errorf("the API key leaked into the error: %v", err)
		}
	}
}

func TestPlunkRejectsBadInput(t *testing.T) {
	p, c := plunkServer(t, 200, `{}`)
	if err := p.Send(context.Background(), "not an address", "s", "b"); err == nil {
		t.Fatal("bad recipient accepted")
	}
	if err := p.Send(context.Background(), "a@example.com", "line one\r\nBcc: x@example.com", "b"); err == nil {
		t.Fatal("header injection in the subject accepted")
	}
	if c.method != "" {
		t.Fatal("a request was sent for invalid input")
	}
	if _, err := NewPlunk("", "", "no-reply@pkasemer.com"); err == nil {
		t.Fatal("empty key accepted")
	}
	if _, err := NewPlunk("", "sk_x", "not an address"); err == nil {
		t.Fatal("bad from address accepted")
	}
	if p2, err := NewPlunk("", "sk_x", "no-reply@pkasemer.com"); err != nil || p2.BaseURL != DefaultPlunkURL {
		t.Fatalf("defaults = %+v %v", p2, err)
	}
}

func TestPlunkHonoursCancellation(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	p, _ := NewPlunk(srv.URL, "sk_x", "no-reply@pkasemer.com")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := p.Send(ctx, "a@example.com", "s", "b"); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("a stuck Plunk blocked the request: %v after %v", err, time.Since(start))
	}
}
