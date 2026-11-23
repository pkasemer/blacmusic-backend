// Package mailer sends transactional email (verification codes).
package mailer

import (
	"context"
	"fmt"
	"log/slog"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// Mailer sends one plain-text message.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// Log writes messages to the log instead of sending them. Development only:
// it lets you read verification codes from `docker compose logs api`.
type Log struct{}

func (Log) Send(_ context.Context, to, subject, body string) error {
	slog.Info("email (not sent: no SMTP configured)", "to", to, "subject", subject, "body", body)
	return nil
}

// SMTP sends through any SMTP server that offers STARTTLS on the given port
// (587 is typical for Amazon SES, Mailgun, Resend and similar). Implicit-TLS
// port 465 is not supported.
type SMTP struct {
	Addr     string // host:port
	Host     string
	User     string
	Password string
	From     string // "Name <addr@example.com>" or a bare address
}

// NewSMTP builds an SMTP mailer.
func NewSMTP(host, port, user, password, from string) (*SMTP, error) {
	if _, err := mail.ParseAddress(from); err != nil {
		return nil, fmt.Errorf("SMTP_FROM: %w", err)
	}
	return &SMTP{Addr: host + ":" + port, Host: host, User: user, Password: password, From: from}, nil
}

func (s *SMTP) Send(ctx context.Context, to, subject, body string) error {
	toAddr, err := mail.ParseAddress(to)
	if err != nil {
		return fmt.Errorf("recipient: %w", err)
	}
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return err
	}
	// Reject anything that could add headers; recipients come from validated
	// registration emails and subjects are constants, this is defence in depth.
	for _, v := range []string{subject, toAddr.Address, from.String()} {
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("invalid header value")
		}
	}
	msg := "From: " + from.String() + "\r\n" +
		"To: " + toAddr.Address + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n" +
		strings.ReplaceAll(body, "\n", "\r\n") + "\r\n"

	var auth smtp.Auth
	if s.User != "" {
		auth = smtp.PlainAuth("", s.User, s.Password, s.Host) // refuses to send credentials without TLS
	}
	done := make(chan error, 1)
	go func() { done <- smtp.SendMail(s.Addr, auth, from.Address, []string{toAddr.Address}, []byte(msg)) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(20 * time.Second):
		return fmt.Errorf("smtp: timed out")
	}
}
