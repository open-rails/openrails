package messages

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/mail"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

var (
	// ErrNoAlertEmail is a test for a merchant without an alert address.
	ErrNoAlertEmail = apperr.New(http.StatusConflict, billing.CodeAlertEmailRequired, "the merchant has no alert_email to send a test message to")
	// ErrTestNotSent is a test the sender did not accept.
	ErrTestNotSent  = apperr.New(http.StatusBadGateway, billing.CodeMessageSendFailed, "the email sender did not accept the test message")
	errTestNoSender = apperr.New(http.StatusServiceUnavailable, billing.CodeServiceUnavailable, "the deployment has no email sender")
)

// Preview renders eventType's message from the merchant's templates for a
// customer of language (empty: the merchant's default), over a sample
// customer and fact from the merchant's own profile. A candidate is
// eventType's template in that language for this render only. The host's
// renderer is not asked.
func (s *Service) Preview(ctx context.Context, eventType, language string, candidate *billing.MessageTemplate) (billing.MessagePreview, error) {
	preview, _, _, err := s.preview(ctx, eventType, language, candidate)
	return preview, err
}

// Test sends Preview's message to the merchant's alert address, the only
// address a test goes to, at most TestCap times a TestWindow.
func (s *Service) Test(ctx context.Context, eventType, language string, candidate *billing.MessageTemplate) (billing.TestMessage, error) {
	preview, content, cfg, err := s.preview(ctx, eventType, language, candidate)
	if err != nil {
		return billing.TestMessage{}, err
	}
	to, err := mail.ParseAddress(strings.TrimSpace(cfg.AlertEmail))
	if err != nil {
		return billing.TestMessage{}, ErrNoAlertEmail
	}
	if s.Email == nil {
		return billing.TestMessage{}, errTestNoSender
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return billing.TestMessage{}, err
	}
	if sent, left := s.Abuse.Count(ctx, "customer_message_test:"+mid.String(), 1, TestWindow); sent > TestCap {
		return billing.TestMessage{}, apperr.New(http.StatusTooManyRequests, billing.CodeRateLimitExceeded,
			fmt.Sprintf("a merchant sends at most %d test messages an hour", TestCap)).
			WithMetadata(map[string]any{"retry_after_seconds": max(int64(math.Ceil(left.Seconds())), 1)})
	}
	err = s.Email.Send(ctx, config.Email{
		From: config.EmailAddress{Name: cfg.Profile.DisplayName, Address: cfg.Profile.FromEmail}, To: to.Address,
		Subject: content.Subject, Text: content.Text, HTML: content.HTML,
	})
	if err != nil {
		return billing.TestMessage{}, fmt.Errorf("%w: %w", ErrTestNotSent, err)
	}
	return billing.TestMessage{Recipient: to.Address, Message: preview}, nil
}

func (s *Service) preview(ctx context.Context, eventType, language string, candidate *billing.MessageTemplate) (billing.MessagePreview, billing.MessageContent, models.MerchantConfiguration, error) {
	var none billing.MessagePreview
	switch {
	case !Known(eventType):
		return none, billing.MessageContent{}, models.MerchantConfiguration{}, apperr.Invalidf("%q is not an event type a message can be about", eventType).WithParam("event_type")
	case language != "" && !languageTag.MatchString(language):
		return none, billing.MessageContent{}, models.MerchantConfiguration{}, apperr.Invalidf("%q is not a language tag", language).WithParam("language")
	}
	settings, err := s.Profiles.Messages(ctx)
	if err != nil {
		return none, billing.MessageContent{}, models.MerchantConfiguration{}, err
	}
	cfg, _, err := s.Profiles.Get(ctx)
	if err != nil && !errors.Is(err, merchantconfig.ErrNoSettings) {
		return none, billing.MessageContent{}, models.MerchantConfiguration{}, err
	}
	if language == "" {
		language = languages(settings, "")[0]
	}
	param := ""
	if candidate != nil {
		settings, param = withTemplate(settings, eventType, language, *candidate), "template"
	}
	f := sample(eventType)
	f.Profile = cfg.Profile
	content, err := Render(settings, language, messageData(f))
	if err != nil {
		refusal := apperr.New(http.StatusBadRequest, billing.CodeMessageTemplateInvalid, err.Error())
		if param != "" {
			refusal = refusal.WithParam(param)
		}
		return none, billing.MessageContent{}, cfg, refusal
	}
	_, _, written, _ := resolve(settings, eventType, language)
	out := billing.MessagePreview{EventType: eventType, Language: written, Subject: content.Subject}
	if content.Text != "" {
		out.Text = &content.Text
	}
	if content.HTML != "" {
		out.HTML = &content.HTML
	}
	return out, content, cfg, nil
}

// withTemplate is s with t as eventType's template in language; s itself,
// which the merchant's configuration may share, is not changed.
func withTemplate(s billing.MessageSettings, eventType, language string, t billing.MessageTemplate) billing.MessageSettings {
	events := maps.Clone(s.Events)
	if events == nil {
		events = map[string]billing.MessageEventSettings{}
	}
	e := events[eventType]
	e.Templates = maps.Clone(e.Templates)
	if e.Templates == nil {
		e.Templates = map[string]billing.MessageTemplate{}
	}
	e.Templates[language] = t
	events[eventType] = e
	s.Events = events
	return s
}
