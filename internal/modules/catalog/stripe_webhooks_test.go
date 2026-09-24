package catalog

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

const oldVersion = "2020-01-01"

func TestReconcileWebhookEndpointPatchesInPlace(t *testing.T) {
	ctx, fake := t.Context(), newFakeStripe(t)
	svc := fake.service()
	events := []string{"invoice.paid", "checkout.session.completed"}
	desired := func(url string, events []string) DesiredWebhookEndpoint {
		return DesiredWebhookEndpoint{URL: url, EnabledEvents: events, HaveSecret: true}
	}

	res, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: "https://a.example/wh", EnabledEvents: events})
	require.NoError(t, err)
	require.Equal(t, WebhookCreated, res.Action)
	require.NotEmpty(t, res.Secret)
	require.Equal(t, stripeapi.APIVersion, fake.endpoints[res.EndpointID].APIVersion)
	id := res.EndpointID

	for _, step := range []struct {
		desired DesiredWebhookEndpoint
		action  WebhookReconcileAction
		updates int
	}{
		{desired("https://a.example/wh", []string{"checkout.session.completed", "invoice.paid"}), WebhookUnchanged, 0},
		{desired("https://b.example/wh", events), WebhookUpdated, 1},
		{desired("https://b.example/wh", append(events, "charge.refunded")), WebhookUpdated, 2},
	} {
		res, err := svc.ReconcileWebhookEndpoint(ctx, step.desired)
		require.NoError(t, err)
		require.Equal(t, step.action, res.Action)
		require.Equal(t, id, res.EndpointID)
		require.Empty(t, res.Secret, "the existing signing secret survives")
		require.Equal(t, step.updates, fake.updates)
	}
	require.Equal(t, 1, fake.creates)
	require.Zero(t, fake.deletes)

	fake.endpoints[id].Status = "disabled"
	res, err = svc.ReconcileWebhookEndpoint(ctx, desired("https://b.example/wh", append(events, "charge.refunded")))
	require.NoError(t, err)
	require.Equal(t, WebhookUpdated, res.Action)
	require.Equal(t, "enabled", fake.endpoints[id].Status)

	_, err = svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: "https://b.example/wh"})
	require.Error(t, err, "events are required")
}

// #856: an api_version bump or an unknown secret ROLLS OVER additively: the
// successor is created first and the predecessor stays enabled until retired.
func TestReconcileWebhookEndpointRollsOverWithoutDeleting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		secret  bool
	}{{"version drift", oldVersion, true}, {"unknown secret", stripeapi.APIVersion, false}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, fake := t.Context(), newFakeStripe(t)
			svc := fake.service()
			now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
			fake.seed("we_old", tc.version, 1, nil)
			desired := DesiredWebhookEndpoint{URL: "https://a.example/wh", EnabledEvents: []string{"invoice.paid"}, HaveSecret: tc.secret, Now: now}

			res, err := svc.ReconcileWebhookEndpoint(ctx, desired)
			require.NoError(t, err)
			require.Equal(t, WebhookRolledOver, res.Action)
			require.NotEmpty(t, res.Secret)
			require.Zero(t, fake.deletes)
			require.Equal(t, "enabled", fake.endpoints["we_old"].Status)
			require.Equal(t, now.Format(time.RFC3339), fake.endpoints["we_old"].Metadata[StripeMetadataSupersededAt])
			require.Equal(t, []string{"we_old"}, res.Superseded)
			require.Equal(t, []SupersededEndpoint{{ID: "we_old", APIVersion: tc.version, Since: now, RetireAfter: now.Add(WebhookRolloverOverlap)}}, res.Legacy)

			// The successor, not the stamped predecessor, is current next pass.
			desired.HaveSecret = true
			again, err := svc.ReconcileWebhookEndpoint(ctx, desired)
			require.NoError(t, err)
			require.Equal(t, WebhookUnchanged, again.Action)
			require.Equal(t, res.EndpointID, again.EndpointID)
			require.Equal(t, 1, fake.creates)

			ret, err := svc.RetireSupersededWebhookEndpoints(ctx, RetireSupersededParams{Now: now.Add(time.Hour)})
			require.NoError(t, err)
			require.Empty(t, ret.Retired)
			require.Len(t, ret.Pending, 1)
			require.Zero(t, fake.deletes, "inside the overlap nothing is retired")

			ret, err = svc.RetireSupersededWebhookEndpoints(ctx, RetireSupersededParams{Now: now.Add(WebhookRolloverOverlap + time.Minute)})
			require.NoError(t, err)
			require.Equal(t, []string{"we_old"}, ret.Retired)
			require.Equal(t, 1, fake.deletes)
			require.NotNil(t, fake.endpoints[res.EndpointID])
		})
	}
}

func TestWebhookEndpointSafetyRails(t *testing.T) {
	ctx := t.Context()
	desired := DesiredWebhookEndpoint{URL: "https://a.example/wh", EnabledEvents: []string{"invoice.paid"}, HaveSecret: true}

	t.Run("retire refuses without a live successor", func(t *testing.T) {
		fake := newFakeStripe(t)
		fake.seed("we_old", oldVersion, 1, map[string]string{StripeMetadataOpenRailsManaged: "true", StripeMetadataSupersededAt: "2020-01-02T00:00:00Z"})
		_, err := fake.service().RetireSupersededWebhookEndpoints(ctx, RetireSupersededParams{})
		require.ErrorContains(t, err, "no live endpoint at api_version")
		fake.seed("we_new", stripeapi.APIVersion, 2, nil)
		fake.endpoints["we_new"].Status = "disabled"
		_, err = fake.service().RetireSupersededWebhookEndpoints(ctx, RetireSupersededParams{})
		require.ErrorContains(t, err, "not enabled")
		require.Zero(t, fake.deletes)
	})
	t.Run("garbled superseded stamp is never retired", func(t *testing.T) {
		fake := newFakeStripe(t)
		fake.seed("we_old", oldVersion, 1, map[string]string{StripeMetadataOpenRailsManaged: "true", StripeMetadataSupersededAt: "yesterday"})
		fake.seed("we_new", stripeapi.APIVersion, 2, nil)
		ret, err := fake.service().RetireSupersededWebhookEndpoints(ctx, RetireSupersededParams{Now: time.Now().Add(365 * 24 * time.Hour)})
		require.NoError(t, err)
		require.Empty(t, ret.Retired)
		require.Zero(t, fake.deletes)
	})
	t.Run("endpoint budget", func(t *testing.T) {
		fake := newFakeStripe(t)
		for i := range maxManagedWebhookEndpoints {
			fake.seed(fmt.Sprintf("we_seed_%d", i), oldVersion, int64(i), nil)
		}
		_, err := fake.service().ReconcileWebhookEndpoint(ctx, desired)
		require.ErrorIs(t, err, ErrWebhookEndpointBudgetExhausted)
		require.Zero(t, fake.creates)
	})
	t.Run("forbid create", func(t *testing.T) {
		fake := newFakeStripe(t)
		forbid := desired
		forbid.ForbidCreate = true
		_, err := fake.service().ReconcileWebhookEndpoint(ctx, forbid)
		require.ErrorIs(t, err, ErrWebhookCreateForbidden)
		fake.seed("we_old", oldVersion, 1, nil)
		_, err = fake.service().ReconcileWebhookEndpoint(ctx, forbid)
		require.ErrorIs(t, err, ErrWebhookCreateForbidden, "rollover is a create too")
		require.Zero(t, fake.creates+fake.deletes+fake.updates)
		fake.endpoints["we_old"].APIVersion = stripeapi.APIVersion
		res, err := fake.service().ReconcileWebhookEndpoint(ctx, forbid)
		require.NoError(t, err)
		require.Equal(t, WebhookUnchanged, res.Action)
	})
	t.Run("unmanaged endpoints are never touched", func(t *testing.T) {
		fake := newFakeStripe(t)
		fake.seed("we_op", oldVersion, 1, map[string]string{})
		res, err := fake.service().ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: "https://ours.example/wh", EnabledEvents: []string{"invoice.paid"}})
		require.NoError(t, err)
		require.Equal(t, WebhookCreated, res.Action)
		require.Zero(t, fake.updates+fake.deletes)
		require.Empty(t, fake.endpoints["we_op"].Metadata)
	})
}

func TestPublicStripeWebhookURL(t *testing.T) {
	cfg := func(base string) *config.Config { return &config.Config{PublicBillingBaseURL: base} }
	_, _, err := PublicStripeWebhookURL(cfg("https://billing.example.com"), " ")
	require.ErrorContains(t, err, "account_id is required")
	_, _, err = PublicStripeWebhookURL(cfg("not a url"), "acct_123")
	require.Error(t, err)
	for base, want := range map[string]string{
		"https://billing.example.com/billing": "https://billing.example.com/billing/v1/webhooks/stripe/acct_123",
		"https://billing.example.com/":        "https://billing.example.com/v1/webhooks/stripe/acct_123",
		"":                                    "",
		"http://billing.example.com":          "", // Stripe needs public https
		"https://localhost:3053":              "",
		"https://169.254.169.254":             "", // SEC-21: link-local is not public
		"https://100.64.0.1":                  "",
	} {
		got, ok, err := PublicStripeWebhookURL(cfg(base), "acct_123")
		require.NoError(t, err, base)
		require.Equal(t, want != "", ok, base)
		require.Equal(t, want, got, base)
	}
}

// Fixture store backed by the real Vault adapter; not a durability test.
type vaultFixture struct {
	values   map[string]map[string]string
	versions map[string]int
}

func newVaultStore() merchants.MerchantSecretStore {
	return merchants.NewVaultSecretStore("secret", &vaultFixture{values: map[string]map[string]string{}, versions: map[string]int{}})
}
func (v *vaultFixture) ReadSecret(_ context.Context, p string) (map[string]string, int, error) {
	return v.values[p], v.versions[p], nil
}
func (v *vaultFixture) WriteSecret(_ context.Context, p string, data map[string]string) (int, error) {
	v.values[p] = data
	v.versions[p]++
	return v.versions[p], nil
}
func (v *vaultFixture) DeleteSecret(_ context.Context, p string) error {
	delete(v.values, p)
	return nil
}
func (v *vaultFixture) ListSecrets(context.Context, string) ([]string, error) { return nil, nil }

// publication models the account-bound publisher contract; PostgreSQL
// workflow tests qualify the real atomic publication.
type publication struct {
	store    merchants.MerchantSecretStore
	id       merchant.ID
	endpoint string
}

func (p *publication) name(key string) string {
	n, _ := merchants.PSPSecretName("stripe", "live", "acct_123", key)
	return n
}
func (p *publication) value(ctx context.Context, key string) string {
	v, _ := p.store.Get(ctx, p.id, p.name(key))
	return v.Value
}
func (p *publication) Load(ctx context.Context) (merchants.StripeWebhookCredentialState, error) {
	return merchants.StripeWebhookCredentialState{SecretKey: p.value(ctx, "secret_key"), CurrentSecret: p.value(ctx, "webhook_signing_secret"),
		PreviousSecret: p.value(ctx, "webhook_signing_secret_previous"), EndpointID: p.endpoint, Writable: true}, nil
}
func (p *publication) Publish(ctx context.Context, endpoint, value string) error {
	if previous := p.value(ctx, "webhook_signing_secret"); previous != "" {
		if _, err := p.store.Put(ctx, p.id, p.name("webhook_signing_secret_previous"), previous); err != nil {
			return err
		}
	}
	_, err := p.store.Put(ctx, p.id, p.name("webhook_signing_secret"), value)
	p.endpoint = endpoint
	return err
}
func (p *publication) RetireOverlap(ctx context.Context) error {
	return p.store.Delete(ctx, p.id, p.name("webhook_signing_secret_previous"))
}

func managedParams(stripeURL string, store merchants.MerchantSecretStore, id merchant.ID) ManagedStripeWebhookParams {
	return ManagedStripeWebhookParams{
		Config:      &config.Config{PublicBillingBaseURL: "https://billing.example.com", ProviderWriteMode: config.ProviderWriteModeFull},
		SecretStore: store, MerchantID: id, ProviderEnvironment: "live", PspID: "acct_123",
		EnabledEvents: []string{"invoice.paid"}, StripeBaseURL: stripeURL,
	}
}

func putSecret(t *testing.T, store merchants.MerchantSecretStore, id merchant.ID, env, key, value string) {
	name, err := merchants.PSPSecretName("stripe", env, "acct_123", key)
	require.NoError(t, err)
	_, err = store.Put(t.Context(), id, name, value)
	require.NoError(t, err)
}

func TestManagedStripeWebhookCustody(t *testing.T) {
	ctx, id := t.Context(), merchant.ID(uuid.New())

	t.Run("publication stores the minted secret", func(t *testing.T) {
		fake, store := newFakeStripe(t), newVaultStore()
		putSecret(t, store, id, "live", "secret_key", "sk_test_123")
		p := managedParams(fake.url, store, id)
		p.Publication = &publication{store: store, id: id}
		res, err := ReconcileManagedStripeWebhook(ctx, p)
		require.NoError(t, err)
		require.Equal(t, WebhookCreated, res.Result.Action)
		require.Equal(t, "https://billing.example.com/v1/webhooks/stripe/acct_123", fake.endpoints[res.Result.EndpointID].URL)
		require.Equal(t, "whsec_fake_0", p.Publication.(*publication).value(ctx, "webhook_signing_secret"))
	})

	// A minted secret with nowhere durable to go is refused before any Stripe
	// mutation: no custody, an ephemeral snapshot, or a secret recorded under
	// another environment of the same account.
	for name, setup := range map[string]func(store merchants.MerchantSecretStore) ManagedStripeWebhookParams{
		"no custody": func(merchants.MerchantSecretStore) ManagedStripeWebhookParams {
			p := managedParams("", nil, merchant.ID{})
			p.SecretKey = "sk_test_123"
			return p
		},
		"snapshot store": func(store merchants.MerchantSecretStore) ManagedStripeWebhookParams {
			putSecret(t, store, id, "live", "secret_key", "sk_test_123")
			return managedParams("", store, id)
		},
		"other environment's secret": func(store merchants.MerchantSecretStore) ManagedStripeWebhookParams {
			putSecret(t, store, id, "live", "secret_key", "sk_test_123")
			putSecret(t, store, id, "test", "webhook_signing_secret", "whsec_still_valid")
			return managedParams("", store, id)
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeStripe(t)
			fake.seed("we_ok", stripeapi.APIVersion, 1, nil)
			fake.endpoints["we_ok"].URL = "https://billing.example.com/v1/webhooks/stripe/acct_123"
			p := setup(merchants.NewMemorySecretStore())
			p.StripeBaseURL = fake.url
			_, err := ReconcileManagedStripeWebhook(ctx, p)
			require.ErrorContains(t, err, "credential backend cannot retain generated webhook secret")
			require.Zero(t, fake.creates+fake.deletes)
		})
	}

	t.Run("read-only store with declared secret keeps existing endpoint", func(t *testing.T) {
		fake, store := newFakeStripe(t), merchants.NewMemorySecretStore()
		putSecret(t, store, id, "live", "secret_key", "sk_test_123")
		putSecret(t, store, id, "live", "webhook_signing_secret", "whsec_from_manifest")
		fake.seed("we_ok", stripeapi.APIVersion, 1, nil)
		fake.endpoints["we_ok"].URL = "https://billing.example.com/v1/webhooks/stripe/acct_123"
		res, err := ReconcileManagedStripeWebhook(ctx, managedParams(fake.url, merchants.NewReadOnlySecretStore(store), id))
		require.NoError(t, err)
		require.Equal(t, WebhookUnchanged, res.Result.Action)
		require.Zero(t, fake.creates+fake.deletes)
	})

	t.Run("unroutable callback url skips", func(t *testing.T) {
		p := managedParams(newFakeStripe(t).url, newVaultStore(), id)
		p.Config.PublicBillingBaseURL = "http://localhost:3053"
		res, err := ReconcileManagedStripeWebhook(ctx, p)
		require.NoError(t, err)
		require.True(t, res.Skipped)
	})
}

// #856 at the registration layer: a version bump retains the outgoing secret
// through the overlap and retires nothing until the kill switch allows it.
func TestManagedStripeWebhookVersionBumpIsGapless(t *testing.T) {
	ctx, id := t.Context(), merchant.ID(uuid.New())
	fake, store := newFakeStripe(t), newVaultStore()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	putSecret(t, store, id, "live", "secret_key", "sk_test_123")
	putSecret(t, store, id, "live", "webhook_signing_secret", "whsec_on_the_old_endpoint")
	fake.seed("we_old", oldVersion, 1, nil)
	fake.endpoints["we_old"].URL = "https://billing.example.com/v1/webhooks/stripe/acct_123"
	pub := &publication{store: store, id: id}
	p := managedParams(fake.url, store, id)
	p.Publication, p.Now = pub, now

	res, err := ReconcileManagedStripeWebhook(ctx, p)
	require.NoError(t, err)
	require.Equal(t, WebhookRolledOver, res.Result.Action)
	require.Zero(t, fake.deletes)
	require.Len(t, res.RetirePending, 1)
	require.Contains(t, res.OperatorAction, "STILL ENABLED")
	require.Equal(t, "whsec_fake_0", pub.value(ctx, "webhook_signing_secret"))
	require.Equal(t, "whsec_on_the_old_endpoint", pub.value(ctx, "webhook_signing_secret_previous"), "in-flight deliveries still verify")

	p.Now = now.Add(WebhookRolloverOverlap + time.Hour)
	res, err = ReconcileManagedStripeWebhook(ctx, p)
	require.NoError(t, err)
	require.Zero(t, fake.deletes)
	require.Contains(t, res.OperatorAction, "kill switch is off")

	p.AllowRetire = true
	res, err = ReconcileManagedStripeWebhook(ctx, p)
	require.NoError(t, err)
	require.Equal(t, []string{"we_old"}, res.Retired)
	require.Empty(t, res.OperatorAction)
	require.Equal(t, 1, fake.deletes)
	require.Empty(t, pub.value(ctx, "webhook_signing_secret_previous"), "overlap secret dropped only after retirement")
}
