package alerting

import (
	"context"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"strings"
	"time"
)

// MailSender preserves the operator alert channel, separate from customer facts.
type MailSender struct {
	sender   config.EmailSender
	profiles *merchantconfig.Store
}

func NewMailSender(sender config.EmailSender, profiles *merchantconfig.Store) *MailSender {
	return &MailSender{sender: sender, profiles: profiles}
}
func (s *MailSender) IsEnabled() bool { return s != nil && s.sender != nil }
func (s *MailSender) SendEmail(ctx context.Context, to, subject, html, plain string) error {
	if !s.IsEnabled() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	from := config.EmailAddress{Name: "OpenRails"}
	if s.profiles != nil {
		if cfg, _, err := s.profiles.Get(ctx); err == nil {
			from.Address = strings.TrimSpace(cfg.Profile.FromEmail)
			if name := strings.TrimSpace(cfg.Profile.DisplayName); name != "" {
				from.Name = name
			}
		}
	}
	return s.sender.Send(ctx, config.Email{From: from, To: to, Subject: subject, HTML: html, Text: plain})
}
