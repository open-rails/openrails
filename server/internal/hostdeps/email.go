package hostdeps

import (
	"errors"
	"strings"

	"github.com/open-rails/helpers/smtp"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/adapters/smtpmail"
	"github.com/open-rails/openrails/server/internal/hostconfig"
)

// NewEmailSender is the email_smtp section's sender. It does not connect.
func NewEmailSender(c hostconfig.SMTPConfig) (openrails.EmailSender, error) {
	from := strings.TrimSpace(c.From)
	if from != "" {
		if _, err := smtp.ParseMailbox(from); err != nil {
			return nil, errors.New("email_smtp.from (EMAIL_SMTP_FROM): " + err.Error())
		}
	}
	sender, err := smtp.New(smtp.Config{Host: c.Host, Port: c.Port, Username: c.Username, Password: c.Password, From: from})
	if err != nil {
		return nil, err
	}
	return smtpmail.EmailSender(sender), nil
}
