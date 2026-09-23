package catalog

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/httpx"
	"github.com/open-rails/openrails/pkg/merchant"
)

// StripeWebhookPublisher owns atomic credential references for one provider account.
type StripeWebhookPublisher interface {
	Load(context.Context) (merchants.StripeWebhookCredentialState, error)
	Publish(context.Context, string, string) error
	RetireOverlap(context.Context) error
}

type ManagedStripeWebhookParams struct {
	Publication StripeWebhookPublisher

	StripeClients       *stripeapi.Factory
	Config              *config.Config
	SecretStore         merchants.MerchantSecretStore
	MerchantID          merchant.ID
	ProviderEnvironment string
	PspID               string
	SecretKey           string
	EnabledEvents       []string
	StripeBaseURL       string
	// Now overrides the rollover clock (tests). Zero = time.Now().
	Now time.Time
	// AllowRetire lets this pass DELETE endpoints that a successor replaced
	// longer ago than RetireOverlap. False (the default, and what the kill
	// switch produces) means the pass is purely additive.
	AllowRetire bool
	// RetireOverlap overrides WebhookRolloverOverlap.
	RetireOverlap time.Duration
}

type ManagedStripeWebhookResult struct {
	Result     WebhookReconcileResult
	Skipped    bool
	SkipReason string
	SecretName string
	WebhookURL string
	// Retired are superseded endpoints this pass deleted (AllowRetire only).
	Retired []string
	// RetirePending are superseded endpoints still inside their overlap window,
	// or held because retirement was not allowed.
	RetirePending []SupersededEndpoint
	// OperatorAction is a non-empty human sentence when the pass needs a person:
	// a rollover is in flight, or the endpoint budget is exhausted.
	OperatorAction string
}

// PublicStripeWebhookURL builds the canonical account-specific callback URL.
// Embedded and standalone hosts publish the same path under their public billing
// mount. The account resolves the merchant; a slug is never callback authority.
func PublicStripeWebhookURL(cfg *config.Config, accountID string) (string, bool, error) {
	base := ""
	if cfg != nil {
		base = strings.TrimSpace(cfg.PublicBillingBaseURL)
	}
	if base == "" {
		return "", false, nil
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return "", false, fmt.Errorf("Stripe webhook account_id is required")
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false, fmt.Errorf("invalid public_billing_base_url %q", base)
	}
	// #SEC-21: Stripe must be able to REACH this endpoint, so it has to be a
	// public https host. The routability rule is the shared outbound policy —
	// the local check here used to miss 169.254/16 and 100.64/10.
	if u.Scheme != "https" {
		return "", false, nil
	}
	if err := (httpx.Policy{}).ValidateURL(base); err != nil {
		return "", false, nil
	}
	parts := []string{"v1", "webhooks", "stripe", accountID}
	out, err := url.JoinPath(strings.TrimRight(base, "/"), parts...)
	if err != nil {
		return "", false, err
	}
	return out, true, nil
}

func ReconcileManagedStripeWebhook(ctx context.Context, p ManagedStripeWebhookParams) (ManagedStripeWebhookResult, error) {
	if p.Config != nil && p.Config.IsLimitedMode() {
		return ManagedStripeWebhookResult{Skipped: true, SkipReason: "provider writes disabled"}, nil
	}
	webhookURL, ok, err := PublicStripeWebhookURL(p.Config, p.PspID)
	if err != nil {
		return ManagedStripeWebhookResult{}, err
	}
	if !ok {
		return ManagedStripeWebhookResult{Skipped: true, SkipReason: "public webhook url not configured"}, nil
	}

	secretKey := strings.TrimSpace(p.SecretKey)
	var secretName, currentSecret, previousSecret, endpointID string
	haveSecret, writable := false, false
	if p.Publication != nil {
		state, err := p.Publication.Load(ctx)
		if err != nil {
			return ManagedStripeWebhookResult{}, err
		}
		secretKey, currentSecret, endpointID, writable = state.SecretKey, state.CurrentSecret, state.EndpointID, state.Writable
		haveSecret = currentSecret != ""
		previousSecret = state.PreviousSecret
		secretName, err = merchants.PSPSecretName("stripe", p.ProviderEnvironment, p.PspID, "webhook_signing_secret")
		if err != nil {
			return ManagedStripeWebhookResult{}, err
		}
	} else if p.SecretStore != nil && !p.MerchantID.IsZero() {
		// Read-only host snapshots do not publish generated secrets. Every managed
		// mutation requires account-bound publication, never direct backend writes.
		var err error
		secretName, err = merchants.PSPSecretName("stripe", p.ProviderEnvironment, p.PspID, "webhook_signing_secret")
		if err != nil {
			return ManagedStripeWebhookResult{}, err
		}
		sec, err := p.SecretStore.Get(ctx, p.MerchantID, secretName)
		if err != nil && !errors.Is(err, merchants.ErrSecretNotFound) {
			return ManagedStripeWebhookResult{}, err
		}
		currentSecret = sec.Value
		haveSecret = currentSecret != ""
		if secretKey == "" {
			name, err := merchants.PSPSecretName("stripe", p.ProviderEnvironment, p.PspID, "secret_key")
			if err != nil {
				return ManagedStripeWebhookResult{}, err
			}
			sec, err = p.SecretStore.Get(ctx, p.MerchantID, name)
			if err != nil && !errors.Is(err, merchants.ErrSecretNotFound) {
				return ManagedStripeWebhookResult{}, err
			}
			secretKey = sec.Value
		}
	}

	if secretKey == "" {
		return ManagedStripeWebhookResult{Skipped: true, SkipReason: "stripe secret key not configured", WebhookURL: webhookURL, SecretName: secretName}, nil
	}

	// Provider-created signing secrets require durable writable custody. A
	// host-declared secret remains usable through a read-only backend.
	cannotPersist := !writable
	if cannotPersist && !haveSecret {
		return ManagedStripeWebhookResult{}, fmt.Errorf("credential backend cannot retain generated webhook secret: declare webhook_signing_secret and register endpoint %s out-of-band", webhookURL)
	}

	rails := railresolve.FixedSet{"stripe": &config.PSPConfig{Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: secretKey}}}
	svc := &StripeCatalogService{StripeClients: p.StripeClients, Config: p.Config, Rails: rails, BaseURL: p.StripeBaseURL}
	var publish func(context.Context, string, string) error
	if writable {
		publish = p.Publication.Publish
	}
	res, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{
		PublishedEndpointID:      endpointID,
		RequirePublishedIdentity: writable || endpointID != "",
		PublishSecret:            publish,

		URL:           webhookURL,
		EnabledEvents: p.EnabledEvents,
		HaveSecret:    haveSecret,
		ForbidCreate:  cannotPersist || previousSecret != "",
		Now:           p.Now,
		RetireOverlap: p.RetireOverlap,
	})
	if errors.Is(err, ErrWebhookCreateForbidden) {
		return ManagedStripeWebhookResult{}, fmt.Errorf("credential backend cannot retain generated webhook secret for endpoint %s: register it out-of-band and declare webhook_signing_secret: %w", webhookURL, err)
	}
	if errors.Is(err, ErrWebhookEndpointBudgetExhausted) {
		return ManagedStripeWebhookResult{
			SecretName: secretName, WebhookURL: webhookURL,
			OperatorAction: fmt.Sprintf("stripe webhook endpoint %s could not roll over: %v — retire the superseded endpoints (they still deliver) before the account reaches Stripe's per-account limit", webhookURL, err),
		}, nil
	}
	if err != nil {
		return ManagedStripeWebhookResult{}, err
	}
	out := ManagedStripeWebhookResult{
		Result: res, SecretName: secretName, WebhookURL: webhookURL,
	}

	out.RetirePending = res.Legacy
	if len(res.Legacy) == 0 {
		if p.AllowRetire && writable && previousSecret != "" && !res.UnqualifiedPredecessors && res.EndpointID == endpointID {
			if err := p.Publication.RetireOverlap(ctx); err != nil {
				return out, err
			}
		}
		return out, nil
	}
	if !p.AllowRetire {
		out.OperatorAction = supersededOperatorAction(webhookURL, res.Legacy, "the destructive-action kill switch is off, so nothing will be deleted automatically")
		return out, nil
	}
	ret, rerr := svc.RetireSupersededWebhookEndpoints(ctx, RetireSupersededParams{Now: p.Now, Overlap: p.RetireOverlap})
	out.Retired, out.RetirePending = ret.Retired, ret.Pending
	if rerr != nil {
		out.OperatorAction = supersededOperatorAction(webhookURL, res.Legacy, rerr.Error())
		return out, nil
	}
	if len(out.Retired) > 0 && len(out.RetirePending) == 0 && writable && !res.UnqualifiedPredecessors {
		if err := p.Publication.RetireOverlap(ctx); err != nil {
			return out, fmt.Errorf("retire published stripe webhook overlap: %w", err)
		}
	}

	if len(out.RetirePending) > 0 {
		out.OperatorAction = supersededOperatorAction(webhookURL, out.RetirePending, "still inside the delivery-overlap window")
	}
	return out, nil
}

func supersededOperatorAction(webhookURL string, legacy []SupersededEndpoint, why string) string {
	ids := make([]string, 0, len(legacy))
	for _, l := range legacy {
		ids = append(ids, fmt.Sprintf("%s (api_version %s, superseded %s, retireable after %s)",
			l.ID, l.APIVersion, l.Since.Format(time.RFC3339), l.RetireAfter.Format(time.RFC3339)))
	}
	return fmt.Sprintf("stripe webhook endpoint %s rolled over to api_version %s; %d superseded endpoint(s) are STILL ENABLED and still delivering: %s — %s",
		webhookURL, stripeapi.APIVersion, len(legacy), strings.Join(ids, "; "), why)
}
