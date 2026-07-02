package catalog

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestPublicStripeWebhookURL(t *testing.T) {
	got, ok, err := PublicStripeWebhookURL(&config.Config{APIURL: "https://billing.example.com/billing"}, "acme", "")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "https://billing.example.com/billing/v1/merchants/acme/webhooks/stripe", got)

	// #641: a set account_id yields the per-account endpoint.
	perAcct, ok, err := PublicStripeWebhookURL(&config.Config{APIURL: "https://billing.example.com/billing"}, "acme", "acct_123")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "https://billing.example.com/billing/v1/merchants/acme/webhooks/stripe/acct_123", perAcct)

	_, ok, err = PublicStripeWebhookURL(&config.Config{APIURL: "http://localhost:3053"}, "acme", "")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestReconcileManagedStripeWebhookStoresRailMerchantAccountSecret(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	store := merchants.NewMemorySecretStore()
	merchantID := merchant.ID(uuid.New())
	secretKeyName, err := merchants.RailMerchantAccountSecretName("stripe", "live", "acct_123", "secret_key")
	require.NoError(t, err)
	webhookName, err := merchants.RailMerchantAccountSecretName("stripe", "live", "acct_123", "webhook_signing_secret")
	require.NoError(t, err)
	_, err = store.Put(ctx, merchantID, secretKeyName, "sk_test_123")
	require.NoError(t, err)

	res, err := ReconcileManagedStripeWebhook(ctx, ManagedStripeWebhookParams{
		Config:                &config.Config{APIURL: "https://billing.example.com"},
		SecretStore:           store,
		MerchantID:            merchantID,
		MerchantSlug:          "acme",
		ProviderEnvironment:   "live",
		RailMerchantAccountID: "acct_123",
		EnabledEvents:         []string{"invoice.paid"},
		StripeBaseURL:         svc.BaseURL,
	})
	require.NoError(t, err)
	require.False(t, res.Skipped)
	require.Equal(t, WebhookCreated, res.Result.Action)

	sec, err := store.Get(ctx, merchantID, webhookName)
	require.NoError(t, err)
	require.Equal(t, "whsec_fake_0", sec.Value)
	require.Equal(t, "https://billing.example.com/v1/merchants/acme/webhooks/stripe/acct_123", fake.endpoints[res.Result.EndpointID].URL)
}

func TestReconcileManagedStripeWebhookStoresConfigRailSecret(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	rail := &config.RailMerchantAccountConfig{Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_123"}}
	rails := config.RailMerchantAccountSet{"stripe": rail}

	res, err := ReconcileManagedStripeWebhook(ctx, ManagedStripeWebhookParams{
		Config:        &config.Config{APIURL: "https://billing.example.com"},
		Rails:         rails,
		EnabledEvents: []string{"invoice.paid"},
		StripeBaseURL: svc.BaseURL,
	})
	require.NoError(t, err)
	require.False(t, res.Skipped)
	require.Equal(t, WebhookCreated, res.Result.Action)
	require.Equal(t, "whsec_fake_0", rail.Stripe.WebhookSecret)
	require.Equal(t, "https://billing.example.com/v1/webhooks/stripe", fake.endpoints[res.Result.EndpointID].URL)
}
