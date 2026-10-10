// Package email is OpenRails' built-in email sender: any SMTP server, so the
// provider is configuration.
package email

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/open-rails/helpers/smtp"

	"github.com/open-rails/openrails/internal/config"
)

// SMTP delivers email through one SMTP server.
type SMTP struct {
	sender *smtp.Sender
	from   config.EmailAddress
}

// NewSMTP validates cfg and returns its sender. It does not connect.
func NewSMTP(cfg config.SMTPConfig) (*SMTP, error) {
	from := trim(cfg.From)
	if from.Address != "" {
		if _, err := smtp.ParseMailbox(from.Address); err != nil {
			return nil, fmt.Errorf("smtp: from: %w", err)
		}
	}
	sender, err := smtp.New(smtp.Config{Host: cfg.Host, Port: cfg.Port, Username: cfg.Username, Password: cfg.Password})
	if err != nil {
		return nil, err
	}
	return &SMTP{sender: sender, from: from}, nil
}

// Send delivers m; a From field left empty is the configured one.
func (s *SMTP) Send(ctx context.Context, m config.Email) error {
	from := trim(m.From)
	if from.Address == "" {
		from.Address = s.from.Address
	}
	if from.Name == "" {
		from.Name = s.from.Name
	}
	if from.Address == "" {
		return errors.New("smtp: no from address: set the merchant's profile from_email or email_smtp.from")
	}
	return s.sender.Send(ctx, smtp.Message{
		From: &mail.Address{Name: from.Name, Address: from.Address},
		To:   strings.TrimSpace(m.To), Subject: m.Subject, Text: m.Text, HTML: m.HTML,
	})
}

// CheckHealth connects, negotiates TLS and authenticates without sending.
func (s *SMTP) CheckHealth(ctx context.Context) error { return s.sender.CheckHealth(ctx) }

func trim(a config.EmailAddress) config.EmailAddress {
	return config.EmailAddress{Name: strings.TrimSpace(a.Name), Address: strings.TrimSpace(a.Address)}
}
