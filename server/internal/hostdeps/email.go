package hostdeps

import (
	"cmp"
	"context"
	"errors"
	"net/mail"
	"strings"

	"github.com/open-rails/helpers/smtp"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/server/internal/hostconfig"
)

// emailSender is helpers/smtp as the engine's EmailSender: a message's From
// is the merchant's profile from_email when it has one, else the
// deployment's.
type emailSender struct {
	sender *smtp.Sender
	from   *mail.Address
}

// NewEmailSender is the email_smtp section's sender. It does not connect.
func NewEmailSender(c hostconfig.SMTPConfig) (openrails.EmailSender, error) {
	var from *mail.Address
	if raw := strings.TrimSpace(c.From); raw != "" {
		var err error
		if from, err = smtp.ParseMailbox(raw); err != nil {
			return nil, errors.New("email_smtp.from (EMAIL_SMTP_FROM): " + err.Error())
		}
	}
	sender, err := smtp.New(smtp.Config{Host: c.Host, Port: c.Port, Username: c.Username, Password: c.Password})
	if err != nil {
		return nil, err
	}
	return &emailSender{sender: sender, from: from}, nil
}

func (s *emailSender) Send(ctx context.Context, m openrails.Email) error {
	from := mail.Address{Name: strings.TrimSpace(m.From.Name), Address: strings.TrimSpace(m.From.Address)}
	if s.from != nil {
		from.Address = cmp.Or(from.Address, s.from.Address)
		from.Name = cmp.Or(from.Name, s.from.Name)
	}
	if from.Address == "" {
		return errors.New("smtp: no from address: set the merchant's profile from_email or email_smtp.from")
	}
	return s.sender.Send(ctx, smtp.Message{From: &from, To: strings.TrimSpace(m.To), Subject: m.Subject, Text: m.Text, HTML: m.HTML})
}

func (s *emailSender) CheckHealth(ctx context.Context) error { return s.sender.CheckHealth(ctx) }
