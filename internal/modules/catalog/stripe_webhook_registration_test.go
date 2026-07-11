package catalog

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
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
	secretKeyName, err := merchants.PSPSecretName("stripe", "live", "acct_123", "secret_key")
	require.NoError(t, err)
	webhookName, err := merchants.PSPSecretName("stripe", "live", "acct_123", "webhook_signing_secret")
	require.NoError(t, err)
	_, err = store.Put(ctx, merchantID, secretKeyName, "sk_test_123")
	require.NoError(t, err)

	res, err := ReconcileManagedStripeWebhook(ctx, ManagedStripeWebhookParams{
		// Mint+persist is mode-2 (api) behavior; manifest mode refuses (#723).
		Config:              &config.Config{APIURL: "https://billing.example.com", ProviderWriteMode: config.ProviderWriteModeFull, MerchantSource: config.MerchantSourceAPI},
		SecretStore:         store,
		MerchantID:          merchantID,
		MerchantSlug:        "acme",
		ProviderEnvironment: "live",
		PspID:               "acct_123",
		EnabledEvents:       []string{"invoice.paid"},
		StripeBaseURL:       svc.BaseURL,
	})
	require.NoError(t, err)
	require.False(t, res.Skipped)
	require.Equal(t, WebhookCreated, res.Result.Action)

	sec, err := store.Get(ctx, merchantID, webhookName)
	require.NoError(t, err)
	require.Equal(t, "whsec_fake_0", sec.Value)
	require.Equal(t, "https://billing.example.com/v1/merchants/acme/webhooks/stripe/acct_123", fake.endpoints[res.Result.EndpointID].URL)
}

// #788: the boot-config rail destination is gone — a minted signing secret
// with no merchant secret-store destination is a hard error (fail closed),
// never a secret silently dropped on the floor.
func TestReconcileManagedStripeWebhookWithoutStoreDestinationFails(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)

	_, err := ReconcileManagedStripeWebhook(ctx, ManagedStripeWebhookParams{
		// Mint is mode-2 (api) behavior; manifest mode refuses (#723).
		Config:        &config.Config{APIURL: "https://billing.example.com", ProviderWriteMode: config.ProviderWriteModeFull, MerchantSource: config.MerchantSourceAPI},
		SecretKey:     "sk_test_123",
		EnabledEvents: []string{"invoice.paid"},
		StripeBaseURL: svc.BaseURL,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "stripe webhook secret destination not configured")
}

// MODE 1 (#723): a managed CREATE would mint a signing secret that only seeds
// process memory and is lost on reboot — refused with a pointed error BEFORE
// any Stripe mutation.
func TestReconcileManagedStripeWebhookManifestModeRefusesMint(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	store := merchants.NewMemorySecretStore()
	merchantID := merchant.ID(uuid.New())
	secretKeyName, err := merchants.PSPSecretName("stripe", "live", "acct_123", "secret_key")
	require.NoError(t, err)
	_, err = store.Put(ctx, merchantID, secretKeyName, "sk_test_123")
	require.NoError(t, err)

	// Empty MerchantSource = manifest (the default).
	_, err = ReconcileManagedStripeWebhook(ctx, ManagedStripeWebhookParams{
		Config:              &config.Config{APIURL: "https://billing.example.com", ProviderWriteMode: config.ProviderWriteModeFull},
		SecretStore:         store,
		MerchantID:          merchantID,
		MerchantSlug:        "acme",
		ProviderEnvironment: "live",
		PspID:               "acct_123",
		EnabledEvents:       []string{"invoice.paid"},
		StripeBaseURL:       svc.BaseURL,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "webhook_signing_secret")
	require.Contains(t, err.Error(), "merchant_source=manifest")
	require.Zero(t, fake.creates, "no endpoint minted")
	require.Zero(t, fake.deletes, "nothing deleted")
}

// MODE 1 + a manifest-declared webhook_signing_secret: finding the existing
// managed endpoint stays a no-op — no mint, no error, secret keeps verifying.
func TestReconcileManagedStripeWebhookManifestModeDeclaredSecretFindsExisting(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	store := merchants.NewMemorySecretStore()
	merchantID := merchant.ID(uuid.New())
	secretKeyName, err := merchants.PSPSecretName("stripe", "live", "acct_123", "secret_key")
	require.NoError(t, err)
	webhookName, err := merchants.PSPSecretName("stripe", "live", "acct_123", "webhook_signing_secret")
	require.NoError(t, err)
	_, err = store.Put(ctx, merchantID, secretKeyName, "sk_test_123")
	require.NoError(t, err)
	_, err = store.Put(ctx, merchantID, webhookName, "whsec_from_manifest")
	require.NoError(t, err)

	// Existing managed endpoint at the pinned version and desired URL/events.
	fake.endpoints["we_ok"] = &StripeWebhookEndpoint{
		ID: "we_ok", URL: "https://billing.example.com/v1/merchants/acme/webhooks/stripe/acct_123", Status: "enabled",
		APIVersion:    stripeapi.APIVersion,
		EnabledEvents: []string{"invoice.paid"},
		Metadata:      map[string]string{StripeMetadataOpenRailsManaged: "true"},
	}

	res, err := ReconcileManagedStripeWebhook(ctx, ManagedStripeWebhookParams{
		Config:              &config.Config{APIURL: "https://billing.example.com", ProviderWriteMode: config.ProviderWriteModeFull},
		SecretStore:         store,
		MerchantID:          merchantID,
		MerchantSlug:        "acme",
		ProviderEnvironment: "live",
		PspID:               "acct_123",
		EnabledEvents:       []string{"invoice.paid"},
		StripeBaseURL:       svc.BaseURL,
	})
	require.NoError(t, err)
	require.False(t, res.Skipped)
	require.Equal(t, WebhookUnchanged, res.Result.Action)
	require.Zero(t, fake.creates)
	require.Zero(t, fake.deletes)

	// The manifest-declared secret is untouched (still the verification key).
	sec, err := store.Get(ctx, merchantID, webhookName)
	require.NoError(t, err)
	require.Equal(t, "whsec_from_manifest", sec.Value)
}
