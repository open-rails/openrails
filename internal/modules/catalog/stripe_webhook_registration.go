package catalog

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

type ManagedStripeWebhookParams struct {
	Config              *config.Config
	Rails               config.ProviderAccountSet
	SecretStore         merchants.MerchantSecretStore
	MerchantID          merchant.ID
	MerchantSlug        string
	ProviderEnvironment string
	ProviderAccountID   string
	SecretKey           string
	EnabledEvents       []string
	StripeBaseURL       string
}

type ManagedStripeWebhookResult struct {
	Result     WebhookReconcileResult
	Skipped    bool
	SkipReason string
	SecretName string
	WebhookURL string
}

// PublicStripeWebhookURL builds the inbound Stripe webhook URL for a merchant.
// When accountID is set it returns the per-account endpoint
// (…/merchants/{slug}/webhooks/stripe/{account_id}, #641) so a merchant with
// multiple Stripe accounts gets one managed endpoint each; empty accountID
// returns the shared …/webhooks/stripe path.
func PublicStripeWebhookURL(cfg *config.Config, merchantSlug, accountID string) (string, bool, error) {
	base := ""
	if cfg != nil {
		base = strings.TrimSpace(cfg.APIURL)
	}
	if base == "" {
		return "", false, nil
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false, fmt.Errorf("invalid api_url %q", base)
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" || host == "localhost" || host == "0.0.0.0" || host == "127.0.0.1" || host == "::1" || net.ParseIP(host) != nil && net.ParseIP(host).IsPrivate() {
		return "", false, nil
	}
	parts := []string{"v1"}
	if strings.TrimSpace(merchantSlug) != "" {
		parts = append(parts, "merchants", merchantSlug, "webhooks", "stripe")
		if id := strings.TrimSpace(accountID); id != "" {
			parts = append(parts, id) // #641 per-account endpoint
		}
	} else {
		parts = append(parts, "webhooks", "stripe")
	}
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
	webhookURL, ok, err := PublicStripeWebhookURL(p.Config, p.MerchantSlug, p.ProviderAccountID)
	if err != nil {
		return ManagedStripeWebhookResult{}, err
	}
	if !ok {
		return ManagedStripeWebhookResult{Skipped: true, SkipReason: "public webhook url not configured"}, nil
	}

	secretKey := strings.TrimSpace(p.SecretKey)
	var secretName string
	haveSecret := false
	if p.SecretStore != nil && !p.MerchantID.IsZero() && strings.TrimSpace(p.ProviderAccountID) != "" {
		keyName, err := merchants.ProviderAccountSecretName("stripe", p.ProviderEnvironment, p.ProviderAccountID, "secret_key")
		if err != nil {
			return ManagedStripeWebhookResult{}, err
		}
		secretName, err = merchants.ProviderAccountSecretName("stripe", p.ProviderEnvironment, p.ProviderAccountID, "webhook_signing_secret")
		if err != nil {
			return ManagedStripeWebhookResult{}, err
		}
		if secretKey == "" {
			sec, err := p.SecretStore.Get(ctx, p.MerchantID, keyName)
			if err != nil && !errors.Is(err, merchants.ErrSecretNotFound) {
				return ManagedStripeWebhookResult{}, fmt.Errorf("load stripe secret key: %w", err)
			}
			secretKey = strings.TrimSpace(sec.Value)
		}
		sec, err := p.SecretStore.Get(ctx, p.MerchantID, secretName)
		if err != nil && !errors.Is(err, merchants.ErrSecretNotFound) {
			return ManagedStripeWebhookResult{}, fmt.Errorf("load stripe webhook secret: %w", err)
		}
		haveSecret = strings.TrimSpace(sec.Value) != ""
	}

	if secretKey == "" {
		if stripeProc := p.Rails.GetStripeRail(); stripeProc != nil && stripeProc.Stripe != nil {
			secretKey = strings.TrimSpace(stripeProc.Stripe.SecretKey)
			haveSecret = haveSecret || strings.TrimSpace(stripeProc.Stripe.WebhookSecret) != ""
		}
	}
	if secretKey == "" {
		return ManagedStripeWebhookResult{Skipped: true, SkipReason: "stripe secret key not configured", WebhookURL: webhookURL, SecretName: secretName}, nil
	}

	rails := config.ProviderAccountSet{"stripe": &config.ProviderAccountConfig{Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: secretKey}}}
	svc := &StripeCatalogService{Config: p.Config, Rails: rails, BaseURL: p.StripeBaseURL}
	res, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{
		URL:           webhookURL,
		EnabledEvents: p.EnabledEvents,
		HaveSecret:    haveSecret,
	})
	if err != nil {
		return ManagedStripeWebhookResult{}, err
	}
	out := ManagedStripeWebhookResult{Result: res, SecretName: secretName, WebhookURL: webhookURL}
	if strings.TrimSpace(res.Secret) == "" {
		return out, nil
	}
	if secretName != "" {
		if _, err := p.SecretStore.Put(ctx, p.MerchantID, secretName, res.Secret); err != nil {
			return ManagedStripeWebhookResult{}, fmt.Errorf("store stripe webhook secret: %w", err)
		}
		return out, nil
	}
	if stripeProc := p.Rails.GetStripeRail(); stripeProc != nil {
		if stripeProc.Stripe == nil {
			stripeProc.Stripe = &config.StripeRailConfig{}
		}
		stripeProc.Stripe.WebhookSecret = res.Secret
		return out, nil
	}
	return ManagedStripeWebhookResult{}, fmt.Errorf("stripe webhook secret destination not configured")
}
