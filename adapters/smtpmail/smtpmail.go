// Package smtpmail sends OpenRails' email through a helpers/smtp sender the
// host opened: smtpmail.EmailSender(s) is Deps.Email.
package smtpmail

import (
	"cmp"
	"context"
	"errors"
	"net/mail"
	"strings"

	"github.com/open-rails/helpers/smtp"

	"github.com/open-rails/openrails"
)

// EmailSender is s as Deps.Email. A message's From is the merchant's
// profile from_email when it has one, else s's Config.From; with neither, Send
// refuses it.
func EmailSender(s *smtp.Sender) openrails.EmailSender { return sender{s} }

type sender struct{ s *smtp.Sender }

func (e sender) Send(ctx context.Context, m openrails.Email) error {
	from := mail.Address{Name: strings.TrimSpace(m.From.Name), Address: strings.TrimSpace(m.From.Address)}
	if d := e.s.From(); d != nil {
		from.Address = cmp.Or(from.Address, d.Address)
		from.Name = cmp.Or(from.Name, d.Name)
	}
	if from.Address == "" {
		return errors.New("smtp: no from address: set the merchant's profile from_email or the sender's From")
	}
	return e.s.Send(ctx, smtp.Message{From: &from, To: strings.TrimSpace(m.To), Subject: m.Subject, Text: m.Text, HTML: m.HTML})
}

func (e sender) CheckHealth(ctx context.Context) error { return e.s.CheckHealth(ctx) }
