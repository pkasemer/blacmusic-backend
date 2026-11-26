package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Expo's push endpoints.
const (
	expoSendURL     = "https://exp.host/--/api/v2/push/send"
	expoReceiptsURL = "https://exp.host/--/api/v2/push/getReceipts"
)

// ExpoMessage is one message in a send request.
type ExpoMessage struct {
	To        string            `json:"to"`
	Title     string            `json:"title"`
	Body      string            `json:"body"`
	Sound     string            `json:"sound"`
	ChannelID string            `json:"channelId"`
	Priority  string            `json:"priority"`
	Data      map[string]string `json:"data"`
}

// Ticket is Expo's answer for one message: accepted (with an id to read the
// receipt with later) or refused.
type Ticket struct {
	OK      bool
	ID      string
	Error   string // Expo's error code, for example DeviceNotRegistered
	Message string
}

// Receipt is the later answer about delivery to Apple/Google.
type Receipt struct {
	OK      bool
	Error   string
	Message string
}

// HTTPError is a request-level failure; Retryable says whether trying again later can help.
type HTTPError struct {
	Status    int
	Body      string
	Retryable bool
}

func (e *HTTPError) Error() string { return fmt.Sprintf("expo answered %d: %s", e.Status, e.Body) }

// Expo talks to Expo's push service over plain HTTPS. AccessToken is optional
// (needed only when "enhanced push security" is switched on for the project).
type Expo struct {
	SendURL, ReceiptsURL string
	AccessToken          string
	HTTP                 *http.Client
}

// NewExpo returns a client for the real service.
func NewExpo(accessToken string) *Expo {
	return &Expo{SendURL: expoSendURL, ReceiptsURL: expoReceiptsURL, AccessToken: accessToken, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (e *Expo) post(ctx context.Context, url string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if e.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+e.AccessToken)
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return &HTTPError{Body: err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		body := string(raw)
		if len(body) > 300 {
			body = body[:300]
		}
		return &HTTPError{Status: resp.StatusCode, Body: body, Retryable: resp.StatusCode == 429 || resp.StatusCode >= 500}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &HTTPError{Status: resp.StatusCode, Body: "unreadable answer", Retryable: true}
	}
	return nil
}

type ticketJSON struct {
	Status  string `json:"status"`
	ID      string `json:"id"`
	Message string `json:"message"`
	Details struct {
		Error string `json:"error"`
	} `json:"details"`
}

// Send sends up to 100 messages and returns one ticket per message, in order.
func (e *Expo) Send(ctx context.Context, msgs []ExpoMessage) ([]Ticket, error) {
	var resp struct {
		Data []ticketJSON `json:"data"`
	}
	if err := e.post(ctx, e.SendURL, msgs, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) != len(msgs) {
		return nil, &HTTPError{Status: 200, Body: fmt.Sprintf("expected %d tickets, got %d", len(msgs), len(resp.Data)), Retryable: false}
	}
	out := make([]Ticket, len(resp.Data))
	for i, t := range resp.Data {
		out[i] = Ticket{OK: t.Status == "ok", ID: t.ID, Error: t.Details.Error, Message: t.Message}
	}
	return out, nil
}

// Receipts reads the delivery receipts for ticket ids. Ids Expo has no receipt for yet are absent.
func (e *Expo) Receipts(ctx context.Context, ids []string) (map[string]Receipt, error) {
	var resp struct {
		Data map[string]ticketJSON `json:"data"`
	}
	if err := e.post(ctx, e.ReceiptsURL, map[string][]string{"ids": ids}, &resp); err != nil {
		return nil, err
	}
	out := make(map[string]Receipt, len(resp.Data))
	for id, t := range resp.Data {
		out[id] = Receipt{OK: t.Status == "ok", Error: t.Details.Error, Message: t.Message}
	}
	return out, nil
}
