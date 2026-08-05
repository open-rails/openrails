package catalog

import (
	"context"
	"testing"
	"time"

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

func TestReconcileManagedStripeWebhookStoresPSPSecret(t *testing.T) {
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

// #856 trigger 2: a psps row's environment (or account_id) changed, so the
// DERIVED secret name moved while the secret sat untouched under the old one.
// The local record is repaired; the remote endpoint is never touched.
func TestReconcileManagedStripeWebhookRepairsDriftedSecretName(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	store := merchants.NewMemorySecretStore()
	merchantID := merchant.ID(uuid.New())

	// Secrets were written while the psps row said environment=test.
	oldWebhookName, err := merchants.PSPSecretName("stripe", "test", "acct_123", "webhook_signing_secret")
	require.NoError(t, err)
	_, err = store.Put(ctx, merchantID, oldWebhookName, "whsec_still_valid")
	require.NoError(t, err)
	liveKeyName, err := merchants.PSPSecretName("stripe", "live", "acct_123", "secret_key")
	require.NoError(t, err)
	_, err = store.Put(ctx, merchantID, liveKeyName, "sk_test_123")
	require.NoError(t, err)

	// The live endpoint exists and is correct.
	fake.endpoints["we_ok"] = &StripeWebhookEndpoint{
		ID: "we_ok", URL: "https://billing.example.com/v1/merchants/acme/webhooks/stripe/acct_123",
		Status: "enabled", APIVersion: stripeapi.APIVersion, Created: 1,
		EnabledEvents: []string{"invoice.paid"},
		Metadata:      map[string]string{StripeMetadataOpenRailsManaged: "true"},
	}

	// The row now reads environment=live: the derived name misses.
	res, err := ReconcileManagedStripeWebhook(ctx, ManagedStripeWebhookParams{
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
	require.Zero(t, fake.deletes, "a derived-name drift NEVER deletes the remote endpoint")
	require.Zero(t, fake.creates, "and never mints a replacement either")
	require.Equal(t, WebhookUnchanged, res.Result.Action)
	require.Equal(t, oldWebhookName, res.RepairedFrom)
	require.Empty(t, res.OperatorAction)

	// The secret is now readable under the CURRENT derived name, unchanged.
	sec, err := store.Get(ctx, merchantID, res.SecretName)
	require.NoError(t, err)
	require.Equal(t, "whsec_still_valid", sec.Value)
}

// #856 trigger 1 end-to-end at the registration layer: an api_version bump
// rolls over with zero deletes, retains the outgoing secret so in-flight
// deliveries on the superseded endpoint still verify, and raises an operator
// action instead of self-deleting.
func TestReconcileManagedStripeWebhookVersionBumpIsGapless(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	store := merchants.NewMemorySecretStore()
	merchantID := merchant.ID(uuid.New())
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{APIURL: "https://billing.example.com", ProviderWriteMode: config.ProviderWriteModeFull, MerchantSource: config.MerchantSourceAPI}

	keyName, err := merchants.PSPSecretName("stripe", "live", "acct_123", "secret_key")
	require.NoError(t, err)
	_, err = store.Put(ctx, merchantID, keyName, "sk_test_123")
	require.NoError(t, err)
	webhookName, err := merchants.PSPSecretName("stripe", "live", "acct_123", "webhook_signing_secret")
	require.NoError(t, err)
	_, err = store.Put(ctx, merchantID, webhookName, "whsec_on_the_old_endpoint")
	require.NoError(t, err)
	previousName, err := merchants.PSPSecretName("stripe", "live", "acct_123", "webhook_signing_secret_previous")
	require.NoError(t, err)

	// An endpoint pinned to a version we no longer ship.
	fake.endpoints["we_old"] = &StripeWebhookEndpoint{
		ID: "we_old", URL: "https://billing.example.com/v1/merchants/acme/webhooks/stripe/acct_123",
		Status: "enabled", APIVersion: "2020-01-01", Created: 1,
		EnabledEvents: []string{"invoice.paid"},
		Metadata:      map[string]string{StripeMetadataOpenRailsManaged: "true"},
	}

	params := ManagedStripeWebhookParams{
		Config: cfg, SecretStore: store, MerchantID: merchantID, MerchantSlug: "acme",
		ProviderEnvironment: "live", PspID: "acct_123",
		EnabledEvents: []string{"invoice.paid"}, StripeBaseURL: svc.BaseURL, Now: now,
	}
	res, err := ReconcileManagedStripeWebhook(ctx, params)
	require.NoError(t, err)
	require.Equal(t, WebhookRolledOver, res.Result.Action)
	require.Zero(t, fake.deletes, "an api_version bump deletes NOTHING")
	require.Equal(t, "enabled", fake.endpoints["we_old"].Status, "the old endpoint keeps delivering")
	require.Len(t, res.RetirePending, 1)
	require.Contains(t, res.OperatorAction, "STILL ENABLED")

	// Both secrets are live: the new one primary, the outgoing one retained, so
	// a delivery already queued on we_old still verifies.
	cur, err := store.Get(ctx, merchantID, webhookName)
	require.NoError(t, err)
	require.Equal(t, "whsec_fake_0", cur.Value)
	prev, err := store.Get(ctx, merchantID, previousName)
	require.NoError(t, err)
	require.Equal(t, "whsec_on_the_old_endpoint", prev.Value)

	// Retirement held: AllowRetire is false (the kill switch default).
	params.Now = now.Add(WebhookRolloverOverlap + time.Hour)
	res, err = ReconcileManagedStripeWebhook(ctx, params)
	require.NoError(t, err)
	require.Zero(t, fake.deletes, "kill switch off: nothing is ever deleted")
	require.Contains(t, res.OperatorAction, "kill switch is off")

	// Armed + past the overlap: the predecessor retires and its secret is dropped.
	params.AllowRetire = true
	res, err = ReconcileManagedStripeWebhook(ctx, params)
	require.NoError(t, err)
	require.Equal(t, []string{"we_old"}, res.Retired)
	require.Equal(t, 1, fake.deletes)
	require.Empty(t, res.OperatorAction)
	_, err = store.Get(ctx, merchantID, previousName)
	require.ErrorIs(t, err, merchants.ErrSecretNotFound)
}
