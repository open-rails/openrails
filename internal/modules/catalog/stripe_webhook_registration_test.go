package catalog

import (
	"context"
	"strings"
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
	got, ok, err := PublicStripeWebhookURL(&config.Config{PublicBillingBaseURL: "https://billing.example.com/billing"}, "")
	require.ErrorContains(t, err, "account_id is required")
	require.False(t, ok)
	require.Empty(t, got)

	// #641: a set account_id yields the per-account endpoint.
	perAcct, ok, err := PublicStripeWebhookURL(&config.Config{PublicBillingBaseURL: "https://billing.example.com/billing"}, "acct_123")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "https://billing.example.com/billing/v1/webhooks/stripe/acct_123", perAcct)

	standalone, ok, err := PublicStripeWebhookURL(&config.Config{PublicBillingBaseURL: "https://billing.example.com"}, "acct_123")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "https://billing.example.com/v1/webhooks/stripe/acct_123", standalone)
	_, ok, err = PublicStripeWebhookURL(&config.Config{PublicBillingBaseURL: "http://localhost:3053"}, "acct_123")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestReconcileManagedStripeWebhookStoresPSPSecret(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	store := newWebhookVaultFixture()
	merchantID := merchant.ID(uuid.New())
	secretKeyName, err := merchants.PSPSecretName("stripe", "live", "acct_123", "secret_key")
	require.NoError(t, err)
	webhookName, err := merchants.PSPSecretName("stripe", "live", "acct_123", "webhook_signing_secret")
	require.NoError(t, err)
	_, err = store.Put(ctx, merchantID, secretKeyName, "sk_test_123")
	require.NoError(t, err)

	res, err := ReconcileManagedStripeWebhook(ctx, ManagedStripeWebhookParams{
		// Minting requires durable credential custody.
		Config:              &config.Config{PublicBillingBaseURL: "https://billing.example.com", ProviderWriteMode: config.ProviderWriteModeFull},
		SecretStore:         store,
		Publication:         &webhookPublicationFixture{store: store, id: merchantID},
		MerchantID:          merchantID,
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
	require.Equal(t, "https://billing.example.com/v1/webhooks/stripe/acct_123", fake.endpoints[res.Result.EndpointID].URL)
}

// #788: the boot-config rail destination is gone — a minted signing secret
// with no merchant secret-store destination is a hard error (fail closed),
// never a secret silently dropped on the floor.
func TestReconcileManagedStripeWebhookWithoutStoreDestinationFails(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)

	_, err := ReconcileManagedStripeWebhook(ctx, ManagedStripeWebhookParams{
		// Missing credential custody fails before any provider mutation.
		Config:        &config.Config{PublicBillingBaseURL: "https://billing.example.com", ProviderWriteMode: config.ProviderWriteModeFull},
		SecretKey:     "sk_test_123",
		PspID:         "acct_123",
		EnabledEvents: []string{"invoice.paid"},
		StripeBaseURL: svc.BaseURL,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "credential backend cannot retain generated webhook secret")
}

// An ephemeral snapshot cannot retain a managed CREATE signing secret: it seeds
// process memory and is lost on reboot — refused with a pointed error BEFORE
// any Stripe mutation.
func TestReconcileManagedStripeWebhookReadOnlyBackendRefusesMint(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	store := merchants.NewMemorySecretStore()
	merchantID := merchant.ID(uuid.New())
	secretKeyName, err := merchants.PSPSecretName("stripe", "live", "acct_123", "secret_key")
	require.NoError(t, err)
	_, err = store.Put(ctx, merchantID, secretKeyName, "sk_test_123")
	require.NoError(t, err)

	// An ephemeral credential snapshot cannot retain a provider-generated secret.
	_, err = ReconcileManagedStripeWebhook(ctx, ManagedStripeWebhookParams{
		Config:              &config.Config{PublicBillingBaseURL: "https://billing.example.com", ProviderWriteMode: config.ProviderWriteModeFull},
		SecretStore:         store,
		MerchantID:          merchantID,
		ProviderEnvironment: "live",
		PspID:               "acct_123",
		EnabledEvents:       []string{"invoice.paid"},
		StripeBaseURL:       svc.BaseURL,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "webhook_signing_secret")
	require.Contains(t, err.Error(), "credential backend cannot retain generated webhook secret")
	require.Zero(t, fake.creates, "no endpoint minted")
	require.Zero(t, fake.deletes, "nothing deleted")
}

// A read-only backend with a declared webhook_signing_secret: finding the existing
// managed endpoint stays a no-op — no mint, no error, secret keeps verifying.
func TestReconcileManagedStripeWebhookDeclaredSecretFindsExisting(t *testing.T) {
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

	store = merchants.NewReadOnlySecretStore(store)

	// Existing managed endpoint at the pinned version and desired URL/events.
	fake.endpoints["we_ok"] = &StripeWebhookEndpoint{
		ID: "we_ok", URL: "https://billing.example.com/v1/webhooks/stripe/acct_123", Status: "enabled",
		APIVersion:    stripeapi.APIVersion,
		EnabledEvents: []string{"invoice.paid"},
		Metadata:      map[string]string{StripeMetadataOpenRailsManaged: "true"},
	}

	res, err := ReconcileManagedStripeWebhook(ctx, ManagedStripeWebhookParams{
		Config:              &config.Config{PublicBillingBaseURL: "https://billing.example.com", ProviderWriteMode: config.ProviderWriteModeFull},
		SecretStore:         store,
		MerchantID:          merchantID,
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

// A secret at another environment or account is not proof of endpoint custody.
func TestReconcileManagedStripeWebhookRefusesUnqualifiedNameRecovery(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	store := newWebhookVaultFixture()
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
		ID: "we_ok", URL: "https://billing.example.com/v1/webhooks/stripe/acct_123",
		Status: "enabled", APIVersion: stripeapi.APIVersion, Created: 1,
		EnabledEvents: []string{"invoice.paid"},
		Metadata:      map[string]string{StripeMetadataOpenRailsManaged: "true"},
	}

	// The row now reads environment=live: the derived name misses.
	_, err = ReconcileManagedStripeWebhook(ctx, ManagedStripeWebhookParams{
		Config:              &config.Config{PublicBillingBaseURL: "https://billing.example.com", ProviderWriteMode: config.ProviderWriteModeFull},
		SecretStore:         store,
		MerchantID:          merchantID,
		ProviderEnvironment: "live",
		PspID:               "acct_123",
		EnabledEvents:       []string{"invoice.paid"},
		StripeBaseURL:       svc.BaseURL,
	})
	require.Error(t, err, "another account or environment is not proof of webhook custody")
	require.Zero(t, fake.deletes)
	require.Zero(t, fake.creates)
}

// #856 trigger 1 end-to-end at the registration layer: an api_version bump
// rolls over with zero deletes, retains the outgoing secret so in-flight
// deliveries on the superseded endpoint still verify, and raises an operator
// action instead of self-deleting.
func TestReconcileManagedStripeWebhookVersionBumpIsGapless(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	store := newWebhookVaultFixture()
	merchantID := merchant.ID(uuid.New())
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{PublicBillingBaseURL: "https://billing.example.com", ProviderWriteMode: config.ProviderWriteModeFull}

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
		ID: "we_old", URL: "https://billing.example.com/v1/webhooks/stripe/acct_123",
		Status: "enabled", APIVersion: "2020-01-01", Created: 1,
		EnabledEvents: []string{"invoice.paid"},
		Metadata:      map[string]string{StripeMetadataOpenRailsManaged: "true"},
	}

	params := ManagedStripeWebhookParams{
		Config: cfg, SecretStore: store, MerchantID: merchantID,
		Publication:         &webhookPublicationFixture{store: store, id: merchantID},
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

// The fake exercises the real Vault-backed store contract without a live Vault.
// Its map is fixture storage only; these tests do not qualify Vault durability.
type webhookVaultFixture struct {
	values   map[string]map[string]string
	versions map[string]int
}

func newWebhookVaultFixture() merchants.MerchantSecretStore {
	return merchants.NewVaultSecretStore("secret", &webhookVaultFixture{values: map[string]map[string]string{}, versions: map[string]int{}})
}
func (v *webhookVaultFixture) ReadSecret(_ context.Context, path string) (map[string]string, int, error) {
	return v.values[path], v.versions[path], nil
}
func (v *webhookVaultFixture) WriteSecret(_ context.Context, path string, data map[string]string) (int, error) {
	v.values[path] = data
	v.versions[path]++
	return v.versions[path], nil
}
func (v *webhookVaultFixture) DeleteSecret(_ context.Context, path string) error {
	delete(v.values, path)
	return nil
}
func (v *webhookVaultFixture) ListSecrets(_ context.Context, prefix string) ([]string, error) {
	var names []string
	for path := range v.values {
		if strings.HasPrefix(path, prefix) {
			names = append(names, strings.TrimPrefix(strings.TrimPrefix(path, prefix), "/"))
		}
	}
	return names, nil
}

// Unit fixture only; PostgreSQL workflow tests qualify atomic publication.
type webhookPublicationFixture struct {
	store    merchants.MerchantSecretStore
	id       merchant.ID
	endpoint string
}

func (p *webhookPublicationFixture) name(key string) string {
	n, _ := merchants.PSPSecretName("stripe", "live", "acct_123", key)
	return n
}
func (p *webhookPublicationFixture) value(ctx context.Context, key string) string {
	v, _ := p.store.Get(ctx, p.id, p.name(key))
	return v.Value
}
func (p *webhookPublicationFixture) Load(ctx context.Context) (merchants.StripeWebhookCredentialState, error) {
	return merchants.StripeWebhookCredentialState{SecretKey: p.value(ctx, "secret_key"), CurrentSecret: p.value(ctx, "webhook_signing_secret"), PreviousSecret: p.value(ctx, "webhook_signing_secret_previous"), EndpointID: p.endpoint, Writable: true}, nil
}
func (p *webhookPublicationFixture) Publish(ctx context.Context, endpoint, value string) error {
	if previous := p.value(ctx, "webhook_signing_secret"); previous != "" {
		if _, err := p.store.Put(ctx, p.id, p.name("webhook_signing_secret_previous"), previous); err != nil {
			return err
		}
	}
	_, err := p.store.Put(ctx, p.id, p.name("webhook_signing_secret"), value)
	if err == nil {
		p.endpoint = endpoint
	}
	return err
}
func (p *webhookPublicationFixture) RetireOverlap(ctx context.Context) error {
	return p.store.Delete(ctx, p.id, p.name("webhook_signing_secret_previous"))
}
