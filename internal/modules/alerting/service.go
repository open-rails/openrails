package alerting

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/internal/shared/httpx"
)

// Deps wires the alerting service.
type Deps struct {
	DB      *db.DB
	Metrics *metrics.Service
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

// Service is the #736 alerting engine: rule/webhook/notification CRUD, the
// evaluator, and test-fire.
type Service struct {
	db               *db.DB
	metrics          *metrics.Service
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
		metrics:          deps.Metrics,
		store:            st,
		deliverer:        newDeliverer(deps.DB, st, deps.Email, deps.Outbound, deps.WebhookBackoff),
		clock:            clock,
		dashboardBaseURL: strings.TrimRight(deps.DashboardBaseURL, "/"),
		outbound:         deps.Outbound,
	}
}

func (s *Service) now() time.Time { return s.clock.Now().UTC() }

// --- rule CRUD (request-path; ctx already merchant-pinned by middleware) ------

// CreateRule validates + persists a new rule. A *ValidationError is returned for
// bad input (template/severity/channels/params).
func (s *Service) CreateRule(ctx context.Context, in CreateRuleInput) (Rule, error) {
	w, verr := s.validateRuleWrite(ctx, in.Template, in.Params, in.Severity, in.Channels, in.Name, in.Enabled)
	if verr != nil {
		return Rule{}, verr
	}
	return s.store.createRule(ctx, w)
}

// UpdateRule patches an existing rule (nil fields unchanged) and re-validates.
func (s *Service) UpdateRule(ctx context.Context, id uuid.UUID, in UpdateRuleInput) (Rule, error) {
	cur, err := s.store.getRule(ctx, id)
	if err != nil {
		return Rule{}, err
	}
	name := cur.Name
	if in.Name != nil {
		name = *in.Name
	}
	params := cur.Params
	if in.Params != nil {
		params = in.Params
	}
	severity := cur.Severity
	if in.Severity != nil {
		severity = *in.Severity
	}
	channels := cur.Channels
	if in.Channels != nil {
		channels = in.Channels
	}
	enabled := cur.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	w, verr := s.validateRuleWrite(ctx, cur.Template, params, severity, channels, name, &enabled)
	if verr != nil {
		return Rule{}, verr
	}
	return s.store.updateRule(ctx, id, w)
}

// GetRule returns one rule.
func (s *Service) GetRule(ctx context.Context, id uuid.UUID) (Rule, error) {
	return s.store.getRule(ctx, id)
}

// ListRules returns the merchant's rules.
func (s *Service) ListRules(ctx context.Context) ([]Rule, error) { return s.store.listRules(ctx) }

// DeleteRule removes a rule; returns false when nothing matched.
func (s *Service) DeleteRule(ctx context.Context, id uuid.UUID) (bool, error) {
	n, err := s.store.deleteRule(ctx, id)
	return n > 0, err
}

// validateRuleWrite resolves + validates the mutable rule fields.
func (s *Service) validateRuleWrite(ctx context.Context, template string, params map[string]any, severity Severity, channels []ChannelRef, name string, enabled *bool) (ruleWrite, *ValidationError) {
	ve := &ValidationError{}
	def, ok := templates[template]
	if !ok {
		return ruleWrite{}, singleFieldError("template", "unknown_template",
			fmt.Sprintf("unknown template %q; valid templates: %v", template, templateKeys()), templateKeys()...)
	}
	if severity == "" {
		severity = def.defaultSeverity
	} else if !severity.valid() {
		ve.add("severity", "invalid", fmt.Sprintf("severity %q must be warning or critical", severity), "warning", "critical")
	}
	if len(channels) == 0 {
		channels = defaultChannels(severity)
	} else {
		s.validateChannels(ctx, channels, ve)
	}
	_, normalized, perr := def.compile(params)
	if perr != nil {
		ve.Errors = append(ve.Errors, perr.Errors...)
	}
	if v := ve.orNil(); v != nil {
		return ruleWrite{}, v
	}
	on := true
	if enabled != nil {
		on = *enabled
	}
	return ruleWrite{name: name, template: template, params: normalized, severity: severity, channels: channels, enabled: on}, nil
}

func (s *Service) validateChannels(ctx context.Context, channels []ChannelRef, ve *ValidationError) {
	for i, ch := range channels {
		field := fmt.Sprintf("channels[%d]", i)
		switch ch.Type {
		case ChannelInApp, ChannelEmail:
			// no ref
		case ChannelWebhook:
			if ch.WebhookID == nil {
				ve.add(field, "missing_webhook_id", "webhook channel requires a webhook_id")
				continue
			}
			if _, err := s.store.getWebhook(ctx, *ch.WebhookID); err != nil {
				ve.add(field, "unknown_webhook", fmt.Sprintf("webhook_id %s does not reference a configured webhook", ch.WebhookID))
			}
		default:
			ve.add(field, "invalid_channel", fmt.Sprintf("unknown channel type %q", ch.Type),
				string(ChannelInApp), string(ChannelEmail), string(ChannelWebhook))
		}
	}
}

// defaultChannels is the severity-based default routing. warning → in_app;
// critical → in_app + email (webhook is added by referencing a configured sink).
func defaultChannels(sev Severity) []ChannelRef {
	if sev == SeverityCritical {
		return []ChannelRef{{Type: ChannelInApp}, {Type: ChannelEmail}}
	}
	return []ChannelRef{{Type: ChannelInApp}}
}

// --- webhook CRUD ------------------------------------------------------------

// CreateWebhook validates + persists an outbound webhook sink.
func (s *Service) CreateWebhook(ctx context.Context, in CreateWebhookInput) (Webhook, error) {
	ve := &ValidationError{}
	if strings.TrimSpace(in.URL) == "" {
		ve.add("url", "required", "url is required")
	} else if err := s.outbound.ValidateURL(in.URL); err != nil {
		// #SEC-21: scheme alone is not validation — a sink that names an
		// internal address turns this endpoint into an SSRF primitive.
		ve.add("url", "invalid_url", "url must be an http(s) URL naming a publicly routable host")
	}
	format := in.Format
	if format == "" {
		format = FormatGeneric
	} else if !format.valid() {
		ve.add("format", "invalid", fmt.Sprintf("format %q must be generic, discord or slack", format),
			string(FormatGeneric), string(FormatDiscord), string(FormatSlack))
	}
	if v := ve.orNil(); v != nil {
		return Webhook{}, v
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	return s.store.createWebhook(ctx, in.Name, in.URL, format, enabled)
}

// ListWebhooks returns the merchant's webhook sinks.
func (s *Service) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	return s.store.listWebhooks(ctx)
}

// DeleteWebhook removes a webhook sink; returns false when nothing matched.
func (s *Service) DeleteWebhook(ctx context.Context, id uuid.UUID) (bool, error) {
	n, err := s.store.deleteWebhook(ctx, id)
	return n > 0, err
}

// --- notifications -----------------------------------------------------------

const notificationListLimit = 100

// ListNotifications returns the bell feed (unreadOnly filters to unread).
func (s *Service) ListNotifications(ctx context.Context, unreadOnly bool) ([]Notification, error) {
	return s.store.listNotifications(ctx, unreadOnly, notificationListLimit)
}

// MarkNotificationRead marks one notification read; returns false when nothing matched.
func (s *Service) MarkNotificationRead(ctx context.Context, id uuid.UUID) (bool, error) {
	n, err := s.store.markNotificationRead(ctx, id)
	return n > 0, err
}

// UnreadCount is the bell badge count.
func (s *Service) UnreadCount(ctx context.Context) (int64, error) { return s.store.unreadCount(ctx) }

// --- test-fire ---------------------------------------------------------------

// TestFireRule sends a clearly-marked TEST alert through the rule's real
// channels, without touching edge state. Returns the per-channel delivery
// results.
func (s *Service) TestFireRule(ctx context.Context, id uuid.UUID) ([]DeliveryResult, error) {
	rule, err := s.store.getRule(ctx, id)
	if err != nil {
		return nil, err
	}
	alert := Alert{
		RuleID:        rule.ID,
		RuleName:      rule.Name,
		Template:      rule.Template,
		Severity:      rule.Severity,
		Metric:        templateMetric(rule.Template),
		Summary:       fmt.Sprintf("Test alert for rule %q — verifying channel delivery.", displayNameForRule(rule)),
		DashboardLink: s.dashboardLink(templateMetric(rule.Template), nil),
		Test:          true,
		FiredAt:       s.now(),
	}
	return s.deliverer.dispatch(ctx, rule, alert), nil
}

// --- dashboard link ----------------------------------------------------------

func (s *Service) dashboardLink(metric string, dims map[string]string) string {
	q := url.Values{}
	if metric != "" {
		q.Set("measure", metric)
	}
	if _, ok := dims["rail_account"]; ok {
		q.Set("by", "rail_account")
	}
	path := "/metrics"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	if s.dashboardBaseURL == "" {
		return path
	}
	return s.dashboardBaseURL + path
}

func templateMetric(template string) string {
	if def, ok := templates[template]; ok {
		return def.metric
	}
	return template
}

func displayNameForRule(rule Rule) string {
	if strings.TrimSpace(rule.Name) != "" {
		return rule.Name
	}
	if def, ok := templates[rule.Template]; ok {
		return def.displayName
	}
	return rule.Template
}
