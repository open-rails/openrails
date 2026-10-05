package alerting

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/httpx"
)

// Deps wires the alerting service.
type Deps struct {
	Secrets merchants.MerchantSecretStore
	DB      *db.DB
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

// Service owns merchant notifications and encrypted outbound webhook destinations.
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
	st.secrets = deps.Secrets
	return &Service{
		db:               deps.DB,
		store:            st,
		deliverer:        newDeliverer(deps.DB, st, deps.Email, deps.Outbound, deps.WebhookBackoff),
		clock:            clock,
		dashboardBaseURL: strings.TrimRight(deps.DashboardBaseURL, "/"),
		outbound:         deps.Outbound,
	}
}

// SetMerchantSecretStore wires the canonical runtime backend before serving.
// The backend refuses webhook writes without configured encryption or Vault.
func (s *Service) SetMerchantSecretStore(secrets merchants.MerchantSecretStore) {
	s.store.secrets = secrets
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

// CreateWebhook validates + persists an outbound webhook sink.
func (s *Service) CreateWebhook(ctx context.Context, in billing.CreateAlertWebhookParams) (billing.AlertWebhook, error) {
	ve := &ValidationError{}
	if strings.TrimSpace(in.URL) == "" {
		ve.add("url", "required", "url is required")
	} else if err := s.outbound.ValidateURL(in.URL); err != nil {
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
	if s.store.secrets == nil {
		return billing.AlertWebhook{}, ErrWebhookCredentialsUnavailable
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return billing.AlertWebhook{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return billing.AlertWebhook{}, err
	}
	rawURL := strings.TrimSpace(in.URL)
	parsed, _ := url.Parse(rawURL)
	secret, err := s.store.secrets.Put(ctx, mid, merchants.AlertWebhookURLSecretName(id), rawURL)
	if err != nil {
		return billing.AlertWebhook{}, err
	}
	version, err := webhookSecretVersion(secret.Version)
	if err != nil {
		return billing.AlertWebhook{}, err
	}
	webhook, err := s.store.createWebhook(ctx, id, in.Name, strings.ToLower(parsed.Host), version, format, enabled)
	if err == nil {
		return webhook.API(), nil
	}
	// A lost database acknowledgement may still have committed. A confirmed
	// row wins; compensate only a rejected insert, never an unknown outcome.
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	committed, readErr := s.store.getWebhook(checkCtx, id)
	if readErr == nil {
		return committed.API(), nil
	}
	var rejected *pgconn.PgError
	if errors.As(err, &rejected) && errors.Is(readErr, pgx.ErrNoRows) {
		if cleanupErr := s.store.secrets.Delete(checkCtx, mid, merchants.AlertWebhookURLSecretName(id)); cleanupErr != nil {
			return billing.AlertWebhook{}, errors.Join(err, cleanupErr)
		}
	}
	return billing.AlertWebhook{}, err
}

// ListWebhooks returns the merchant's webhook sinks.
func (s *Service) ListWebhooks(ctx context.Context) ([]billing.AlertWebhook, error) {
	hooks, err := s.store.listWebhooks(ctx)
	out := make([]billing.AlertWebhook, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, h.API())
	}
	return out, err
}

// DeleteWebhook removes a webhook sink; returns false when nothing matched.
func (s *Service) DeleteWebhook(ctx context.Context, webhookID billing.AlertWebhookID) (bool, error) {
	id := webhookID.UUID()
	if s.store.secrets == nil {
		return false, ErrWebhookCredentialsUnavailable
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	n, err := s.store.deleteWebhook(ctx, id)
	if err != nil {
		return false, err
	}
	// The name is deterministic, so retry can remove an orphan even if the
	// metadata deletion committed before a secret backend outage.
	if err := s.store.secrets.Delete(ctx, mid, merchants.AlertWebhookURLSecretName(id)); err != nil {
		return false, err
	}
	return n > 0, nil
}

var ErrWebhookCredentialsUnavailable = errors.New("encrypted webhook credentials are unavailable")
var ErrWebhookRotationConflict = errors.New("webhook was rotated concurrently; reload and retry")

func webhookSecretVersion(version int) (int32, error) {
	if version < 1 || int64(version) > math.MaxInt32 {
		return 0, ErrWebhookCredentialsUnavailable
	}
	return int32(version), nil
}

// SetWebhookURL replaces only the credential, retaining the webhook and all
// delivery identity. A failed metadata write leaves delivery fail-closed on the
// version mismatch; retrying the same URL repairs that pending rotation.
func (s *Service) SetWebhookURL(ctx context.Context, webhookID billing.AlertWebhookID, in billing.SetAlertWebhookURLParams) (billing.AlertWebhook, error) {
	id := webhookID.UUID()
	rawURL := strings.TrimSpace(in.URL)
	if rawURL == "" || s.outbound.ValidateURL(rawURL) != nil {
		return billing.AlertWebhook{}, singleFieldError("url", "invalid_url", "url must be an http(s) URL naming a publicly routable host")
	}
	if _, err := s.store.getWebhook(ctx, id); err != nil {
		return billing.AlertWebhook{}, err
	}
	if s.store.secrets == nil {
		return billing.AlertWebhook{}, ErrWebhookCredentialsUnavailable
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return billing.AlertWebhook{}, err
	}
	secret, err := s.store.secrets.Put(ctx, mid, merchants.AlertWebhookURLSecretName(id), rawURL)
	if err != nil {
		return billing.AlertWebhook{}, err
	}
	version, err := webhookSecretVersion(secret.Version)
	if err != nil {
		return billing.AlertWebhook{}, err
	}
	parsed, _ := url.Parse(rawURL)
	webhook, err := s.store.rotateWebhookURL(ctx, id, strings.ToLower(parsed.Host), version)
	if !errors.Is(err, pgx.ErrNoRows) {
		return webhook.API(), err
	}
	// Never delete a newer winning rotation. A removed resource can be cleaned
	// by its deterministic name; official creation never reuses a webhook id.
	if _, readErr := s.store.getWebhook(ctx, id); readErr == nil {
		return billing.AlertWebhook{}, ErrWebhookRotationConflict
	} else if !errors.Is(readErr, pgx.ErrNoRows) {
		return billing.AlertWebhook{}, readErr
	}
	if cleanupErr := s.store.secrets.Delete(ctx, mid, merchants.AlertWebhookURLSecretName(id)); cleanupErr != nil {
		return billing.AlertWebhook{}, cleanupErr
	}
	return billing.AlertWebhook{}, pgx.ErrNoRows
}

// --- notifications -----------------------------------------------------------

// ListNotifications pages the merchant's inbox, newest first.
func (s *Service) ListNotifications(ctx context.Context, req billing.MerchantNotificationListParams) (billing.ListPage[billing.MerchantNotification], error) {
	limit, err := pagination.Limit(req.PageRequest)
	if err != nil {
		return billing.ListPage[billing.MerchantNotification]{}, err
	}
	afterAt, afterID, err := pagination.After(req.Cursor)
	if err != nil {
		return billing.ListPage[billing.MerchantNotification]{}, err
	}
	rows, err := s.store.listNotifications(ctx, req.UnreadOnly, afterAt, afterID, pagination.Fetch(limit))
	if err != nil {
		return billing.ListPage[billing.MerchantNotification]{}, err
	}
	page := pagination.Cut(rows, limit, func(r gen.BillingNotification) any { return pagination.TimeID{At: r.CreatedAt, ID: r.ID} })
	return pagination.Map(page, notificationFromRow), nil
}

// MarkNotificationRead marks one notification read and returns it; an
// unknown id is pgx.ErrNoRows.
func (s *Service) MarkNotificationRead(ctx context.Context, id billing.NotificationID) (billing.MerchantNotification, error) {
	row, err := s.store.markNotificationRead(ctx, id.UUID())
	if err != nil {
		return billing.MerchantNotification{}, err
	}
	return notificationFromRow(row), nil
}

// UnreadCount is the bell badge count.
func (s *Service) UnreadCount(ctx context.Context) (int64, error) { return s.store.unreadCount(ctx) }
