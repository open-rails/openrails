package alerting

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/shared/httpx"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Deps wires the alerting service.
type Deps struct {
	DB *db.DB
	// Email is the outbound alert-email sender seam (may be nil — email channel
	// then fails soft with a note in the delivery record).
	Email EmailSender
	// Outbound is the SSRF policy for merchant-supplied webhook sinks (#SEC-21).
	// The zero value is the strict production policy: publicly routable
	// destinations only, enforced at the dialer and on every redirect hop.
	Outbound httpx.Policy
	Clock    clockwork.Clock
	// DashboardBaseURL prefixes alert dashboard deep links (may be empty → the
	// link is a relative path the console resolves).
	DashboardBaseURL string
	// WebhookBackoff is the base delay between webhook retry attempts.
	WebhookBackoff time.Duration
}

// Service owns merchant notifications and the merchant's alert webhooks.
type Service struct {
	db               *db.DB
	store            *store
	deliverer        *deliverer
	clock            clockwork.Clock
	dashboardBaseURL string
	outbound         httpx.Policy
}

// NewService builds the alerting service.
func NewService(deps Deps) *Service {
	clock := deps.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	st := newStore(deps.DB)
	return &Service{
		db:               deps.DB,
		store:            st,
		deliverer:        newDeliverer(deps.DB, st, deps.Email, deps.Outbound, deps.WebhookBackoff),
		clock:            clock,
		dashboardBaseURL: strings.TrimRight(deps.DashboardBaseURL, "/"),
		outbound:         deps.Outbound,
	}
}

// SetMerchantConfig binds the merchant configuration webhooks live in.
func (s *Service) SetMerchantConfig(config *merchantdocs.Cache) {
	s.store.config = config
}

func (s *Service) now() time.Time { return s.clock.Now().UTC() }

// defaultChannels selects external email delivery after the console notification
// is durable. Enabled webhooks are added by the finding notification path.
func defaultChannels(sev Severity) []ChannelRef {
	if sev == SeverityCritical {
		return []ChannelRef{{Type: ChannelEmail}}
	}
	return nil
}

// --- webhook CRUD ------------------------------------------------------------

// CreateWebhook validates and adds an alert webhook to the merchant document.
func (s *Service) CreateWebhook(ctx context.Context, in billing.CreateAlertWebhookParams) (billing.AlertWebhook, error) {
	ve := &ValidationError{}
	rawURL := strings.TrimSpace(in.URL)
	if rawURL == "" {
		ve.add("url", "required", "url is required")
	} else if err := s.outbound.ValidateURL(rawURL); err != nil {
		// #SEC-21: scheme alone is not validation — a sink that names an
		// internal address turns this endpoint into an SSRF primitive.
		ve.add("url", "invalid_url", "url must be an http(s) URL naming a publicly routable host")
	}
	format := WebhookFormat(in.Format)
	if format == "" {
		format = FormatGeneric
	} else if !format.valid() {
		ve.add("format", "invalid", fmt.Sprintf("format %q must be generic, discord or slack", format),
			string(FormatGeneric), string(FormatDiscord), string(FormatSlack))
	}
	if v := ve.orNil(); v != nil {
		return billing.AlertWebhook{}, v
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return billing.AlertWebhook{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return billing.AlertWebhook{}, err
	}
	now := s.now()
	hook := merchantdocs.AlertWebhook{
		ID: billing.AlertWebhookID(id), Name: strings.TrimSpace(in.Name), URL: rawURL,
		Format: billing.AlertWebhookFormat(format), Enabled: enabled, CreatedAt: now, UpdatedAt: now,
	}
	err = s.store.editWebhooks(ctx, func(hooks []merchantdocs.AlertWebhook) ([]merchantdocs.AlertWebhook, error) {
		return append(hooks, hook), nil
	})
	if err != nil {
		return billing.AlertWebhook{}, err
	}
	return webhookOf(mid, hook).API(), nil
}

// ListWebhooks returns the merchant's webhook sinks.
func (s *Service) ListWebhooks(ctx context.Context, params billing.AlertWebhookListParams) ([]billing.AlertWebhook, error) {
	hooks, err := s.store.listWebhooks(ctx, uuidutil.Of(params.IDs))
	out := make([]billing.AlertWebhook, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, h.API())
	}
	return out, err
}

// DeleteWebhook removes a webhook sink; returns false when nothing matched.
func (s *Service) DeleteWebhook(ctx context.Context, webhookID billing.AlertWebhookID) (bool, error) {
	deleted := false
	err := s.store.editWebhooks(ctx, func(hooks []merchantdocs.AlertWebhook) ([]merchantdocs.AlertWebhook, error) {
		before := len(hooks)
		hooks = slices.DeleteFunc(hooks, func(h merchantdocs.AlertWebhook) bool { return h.ID == webhookID })
		deleted = len(hooks) != before
		if !deleted {
			return nil, ErrWebhookNotFound
		}
		return hooks, nil
	})
	if errors.Is(err, ErrWebhookNotFound) {
		return false, nil
	}
	return deleted, err
}

// UpdateWebhook changes a webhook's fields; omitted ones keep their values
// and a null name clears it.
func (s *Service) UpdateWebhook(ctx context.Context, webhookID billing.AlertWebhookID, in billing.UpdateAlertWebhookParams) (billing.AlertWebhook, error) {
	ve := &ValidationError{}
	rawURL := strings.TrimSpace(in.URL.Value)
	if in.URL.Set && (in.URL.Null || rawURL == "" || s.outbound.ValidateURL(rawURL) != nil) {
		ve.add("url", "invalid_url", "url must be an http(s) URL naming a publicly routable host")
	}
	if in.Format.Set && (in.Format.Null || !WebhookFormat(in.Format.Value).valid()) {
		ve.add("format", "invalid", fmt.Sprintf("format %q must be generic, discord or slack", in.Format.Value),
			string(FormatGeneric), string(FormatDiscord), string(FormatSlack))
	}
	if in.Enabled.Set && in.Enabled.Null {
		ve.add("enabled", "invalid", "enabled must be true or false")
	}
	if v := ve.orNil(); v != nil {
		return billing.AlertWebhook{}, v
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return billing.AlertWebhook{}, err
	}
	var updated merchantdocs.AlertWebhook
	err = s.store.editWebhooks(ctx, func(hooks []merchantdocs.AlertWebhook) ([]merchantdocs.AlertWebhook, error) {
		i := slices.IndexFunc(hooks, func(h merchantdocs.AlertWebhook) bool { return h.ID == webhookID })
		if i < 0 {
			return nil, ErrWebhookNotFound
		}
		h := hooks[i]
		if in.Name.Set {
			h.Name = strings.TrimSpace(in.Name.Value)
		}
		if in.URL.Set {
			h.URL = rawURL
		}
		if in.Format.Set {
			h.Format = in.Format.Value
		}
		if in.Enabled.Set {
			h.Enabled = in.Enabled.Value
		}
		h.UpdatedAt = s.now()
		hooks[i], updated = h, h
		return hooks, nil
	})
	if err != nil {
		return billing.AlertWebhook{}, err
	}
	return webhookOf(mid, updated).API(), nil
}
