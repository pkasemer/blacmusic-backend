package mailer

import (
	"errors"
	"log/slog"

	"github.com/blacmusic/backend/internal/config"
)

// FromConfig picks how email is delivered: Plunk when its key is set, else SMTP when a host is set, else
// (development only) the log.
func FromConfig(cfg *config.Config) (Mailer, error) {
	switch {
	case cfg.PlunkAPIKey != "":
		m, err := NewPlunk(cfg.PlunkURL, cfg.PlunkAPIKey, cfg.MailFrom)
		if err != nil {
			return nil, err
		}
		slog.Info("emails are sent through Plunk")
		return m, nil
	case cfg.SMTPHost != "":
		return NewSMTP(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUser, cfg.SMTPPassword, cfg.SMTPFrom)
	case cfg.Env != "development":
		return nil, errors.New("PLUNK_API_KEY or SMTP_HOST must be set outside development")
	}
	return Log{}, nil
}
