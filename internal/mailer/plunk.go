package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/mail"
	"strings"
	"time"
)

// DefaultPlunkURL is where Plunk's API lives.
const DefaultPlunkURL = "https://next-api.useplunk.com"

// Plunk sends through Plunk's transactional API (POST /v1/send with a secret
// key). The sender domain must be verified in the Plunk dashboard.
type Plunk struct {
	BaseURL string
	APIKey  string // secret key (sk_...)
	From    mail.Address
	HTTP    *http.Client
}

// NewPlunk builds a Plunk mailer. baseURL may be empty (the default is used);
// from is "Name <addr@example.com>" or a bare address.
func NewPlunk(baseURL, apiKey, from string) (*Plunk, error) {
	addr, err := mail.ParseAddress(from)
	if err != nil {
		return nil, fmt.Errorf("mail from address: %w", err)
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("PLUNK_API_KEY is empty")
	}
	if baseURL == "" {
		baseURL = DefaultPlunkURL
	}
	return &Plunk{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, From: *addr, HTTP: &http.Client{Timeout: 20 * time.Second}}, nil
}

type plunkAddress struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

type plunkRequest struct {
	To         string       `json:"to"`
	Subject    string       `json:"subject"`
	Body       string       `json:"body"` // HTML
	From       plunkAddress `json:"from"`
	Subscribed bool         `json:"subscribed"`
}

// plainToHTML turns a plain-text message into safe HTML: Plunk's body is HTML,
// so text must be escaped and its line breaks kept.
func plainToHTML(text string) string {
	text = strings.ReplaceAll(strings.TrimSpace(text), "\r\n", "\n")
	esc := html.EscapeString(text)
	esc = strings.ReplaceAll(esc, "\n\n", "</p><p>")
	esc = strings.ReplaceAll(esc, "\n", "<br>")
	return `<div style="font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;font-size:16px;line-height:1.5"><p>` + esc + `</p></div>`
}

// Error is a refusal from Plunk; the message never contains the API key.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("plunk answered %d %s: %s", e.Status, e.Code, e.Message)
}

// Send sends one message. Transient failures are reported, not retried: the
// caller (a verification-code request) simply lets the user ask again.
func (p *Plunk) Send(ctx context.Context, to, subject, body string) error {
	toAddr, err := mail.ParseAddress(to)
	if err != nil {
		return fmt.Errorf("recipient: %w", err)
	}
	if strings.ContainsAny(subject, "\r\n") {
		return fmt.Errorf("invalid subject")
	}
	payload, err := json.Marshal(plunkRequest{
		To: toAddr.Address, Subject: subject, Body: plainToHTML(body),
		From: plunkAddress{Name: p.From.Name, Email: p.From.Address}, Subscribed: false,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/send", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("plunk: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	e := &Error{Status: resp.StatusCode}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil {
		e.Code, e.Message = env.Error.Code, env.Error.Message
	}
	if e.Message == "" {
		e.Message = "no details"
	}
	return e
}
