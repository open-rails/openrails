//go:build greenfield && integration

package greenfield_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/vaultfake"
)

const transitKey = "greenfield-solana"

// heldNMI answers NMI's sandbox posture probe only after release: a provider
// that is slow or down while the runtime starts.
type heldNMI struct{ release chan struct{} }

func (h heldNMI) RoundTrip(r *http.Request) (*http.Response, error) {
	select {
	case <-h.release:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	body := `{"object":"transaction","id":"probe_1","response":"1","response_code":"100","response_text":"SUCCESS"}`
	if !strings.HasSuffix(r.URL.Path, "/payments/auth") && !strings.HasSuffix(r.URL.Path, "/void") {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
	}
	return jsonResponse(body), nil
}

type resilientBoot struct {
	vault  *vaultfake.Server
	nmi    http.RoundTripper
	stripe http.RoundTripper
	slug   string
	// redisDown hands the runtime an unreachable Redis.
	redisDown bool
}

func (f *fixture) resilientRuntime(t *testing.T, b resilientBoot) *embed.Runtime {
	t.Helper()
	var rdb *redis.Client
	if b.redisDown {
		rdb = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
		t.Cleanup(func() { _ = rdb.Close() })
	}
	psps := map[string]embed.PSPConfig{
		"stripe": {"stripe": {AccountID: "acct_greenfield", Secrets: map[string]string{"secret_key": "sk_test_greenfield", "webhook_signing_secret": "whsec_greenfield"}}},
		"solana": {"solana": {Signer: &embed.PSPSignerConfig{Mode: "vault_transit", Key: transitKey}}},
	}
	if b.nmi != nil {
		psps["nmi"] = embed.PSPConfig{"nmi": {AccountID: "greenfield-nmi", Secrets: map[string]string{"security_key": "greenfield-nmi-key", "webhook_signing_secret": "whsec_nmi"}, Settings: map[string]any{"tokenization_key": "greenfield-tokenization"}}}
	}
	start := time.Now()
	rt, err := embed.New(t.Context(), embed.Options{
		Config: &config.Config{
			TestMode:            config.CredentialPostureSandbox,
			AllowCatalogUpdates: true,
			ProviderWriteMode:   config.ProviderWriteModeFull,
			DB:                  &config.DBConfig{URL: f.dsn(t), Schema: f.schema},
			ReturnOrigins:       []string{"https://greenfield.test"},
			Vault:               &config.VaultConfig{Enabled: true, Address: b.vault.URL(), Token: b.vault.Token},
			ProviderSandbox:     &config.ProviderSandboxConfig{SolanaRPCURL: "http://127.0.0.1:1"},
		},
		Merchant:        &embed.MerchantDeclaration{Slug: b.slug, Config: embed.MerchantConfig{DisplayName: b.slug, PSPs: psps}},
		PGXPool:         f.pool,
		Redis:           rdb,
		River:           embed.RiverManagedByOpenRails(f.schema),
		RunWorkers:      true,
		StripeTransport: b.stripe,
		NMITransport:    b.nmi,
	})
	require.NoError(t, err)
	require.Less(t, time.Since(start), 20*time.Second, "construction never waits on an optional provider")
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	return rt
}

func probe(t *testing.T, rt *embed.Runtime, name string) error {
	t.Helper()
	for _, p := range rt.Probes() {
		if p.Name == name {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			return p.Check(ctx)
		}
	}
	t.Fatalf("probe %s not registered", name)
	return nil
}

func checkoutPSP(t *testing.T, client *openrails.Client, rail string) (openrails.CheckoutPSPConfig, bool) {
	t.Helper()
	cfg, err := client.GetCheckoutConfig(t.Context())
	require.NoError(t, err)
	for _, psp := range cfg.PSPs {
		if psp.Rail == rail {
			return psp, true
		}
	}
	return openrails.CheckoutPSPConfig{}, false
}

func sign(t *testing.T, rt *embed.Runtime) ([]byte, error) {
	t.Helper()
	return app.HostGraph(rt).Runtime.MerchantSecretBackend.SolanaTransit.Sign(t.Context(), transitKey, []byte("greenfield"))
}

func waitReady(t *testing.T, rt *embed.Runtime) {
	t.Helper()
	require.Eventually(t, func() bool { return rt.Ready(t.Context()) == nil }, 10*time.Second, 20*time.Millisecond)
}

func stripeCheckout(t *testing.T, client *openrails.Client) {
	t.Helper()
	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "post-" + uuid.NewString()[:8], DisplayName: "Post", EntitlementsSpec: map[string]*int{"content:post": nil}})
	require.NoError(t, err)
	price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)
	session, err := client.CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
		Customer:       openrails.CheckoutCustomerIdentity{ID: uuid.NewString(), VerifiedEmail: "reader@example.test"},
		PriceKey:       price.Key,
		Entitlement:    "content:post",
		OfferKind:      openrails.OfferPermanent,
		PaymentOptions: openrails.CheckoutPaymentOptions{Rail: "stripe"},
		IdempotencyKey: "checkout-" + uuid.NewString(),
		SuccessURL:     "https://greenfield.test/success",
		CancelURL:      "https://greenfield.test/cancel",
	})
	require.NoError(t, err)
	require.Equal(t, "stripe", session.RailData["rail"])
}

// First boot with Vault, Redis and the NMI posture probe all unavailable: the
// runtime builds and is ready, the card rails are offered, only the Solana
// rail is missing and its signer answers unavailable; each recovers in the
// background. (Checkout idempotency still needs Redis until #1099.)
func TestOptionalProvidersNeverBlockBoot(t *testing.T) {
	t.Setenv("VAULT_MAX_RETRIES", "0")
	f := newFixture(t)
	fake := vaultfake.New("greenfield-root")
	t.Cleanup(fake.Close)
	fake.SetUp(false)
	nmi := heldNMI{release: make(chan struct{})}
	released := false
	t.Cleanup(func() {
		if !released {
			close(nmi.release)
		}
	})

	rt := f.resilientRuntime(t, resilientBoot{vault: fake, nmi: nmi, stripe: &stripeCheckoutFake{t: t}, slug: "resilient-" + uuid.NewString()[:8], redisDown: true})
	client, err := rt.Client()
	require.NoError(t, err)
	waitReady(t, rt)
	require.ErrorIs(t, probe(t, rt, "openrails_vault"), vault.ErrUnavailable)
	require.Error(t, probe(t, rt, "openrails_psp_posture"), "the NMI verdict is still unknown")

	for _, rail := range []string{"stripe", "nmi"} {
		_, offered := checkoutPSP(t, client, rail)
		require.True(t, offered, rail)
	}
	_, armed := checkoutPSP(t, client, "solana")
	require.False(t, armed, "a first boot without Vault leaves only the Solana rail disarmed")
	_, err = sign(t, rt)
	require.ErrorIs(t, err, vault.ErrUnavailable)

	fake.SetUp(true)
	require.Eventually(t, func() bool { _, ok := checkoutPSP(t, client, "solana"); return ok }, 30*time.Second, 50*time.Millisecond, "the Solana rail arms once Vault answers")
	require.NoError(t, probe(t, rt, "openrails_vault"))
	signature, err := sign(t, rt)
	require.NoError(t, err)
	require.Len(t, signature, 64)

	released = true
	close(nmi.release)
	require.Eventually(t, func() bool { return probe(t, rt, "openrails_psp_posture") == nil }, 30*time.Second, 50*time.Millisecond, "posture verifies in the background")
	require.NoError(t, rt.Ready(t.Context()))
}

// A restart while Vault is down reuses the Solana identity stored on the
// first boot and sells through Stripe; signing waits for Vault and resumes
// with the same key.
func TestStoredSolanaIdentityServesWhileVaultIsDown(t *testing.T) {
	t.Setenv("VAULT_MAX_RETRIES", "0")
	f := newFixture(t)
	fake := vaultfake.New("greenfield-root")
	t.Cleanup(fake.Close)
	slug := "restart-" + uuid.NewString()[:8]
	want := solanago.PublicKeyFromBytes(fake.PublicKey(transitKey)).String()

	first := f.resilientRuntime(t, resilientBoot{vault: fake, stripe: &stripeCheckoutFake{t: t}, slug: slug})
	client, err := first.Client()
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, ok := checkoutPSP(t, client, "solana"); return ok }, 30*time.Second, 50*time.Millisecond)
	stored, _ := checkoutPSP(t, client, "solana")
	require.NoError(t, first.Close(context.Background()))

	fake.SetUp(false)
	second := f.resilientRuntime(t, resilientBoot{vault: fake, stripe: &stripeCheckoutFake{t: t}, slug: slug})
	client, err = second.Client()
	require.NoError(t, err)
	waitReady(t, second)
	again, ok := checkoutPSP(t, client, "solana")
	require.True(t, ok, "the stored identity keeps the rail provisioned")
	require.Equal(t, stored.PSPID, again.PSPID)
	_, err = sign(t, second)
	require.ErrorIs(t, err, vault.ErrUnavailable)
	stripeCheckout(t, client)

	fake.SetUp(true)
	require.Eventually(t, func() bool { _, err := sign(t, second); return err == nil }, 30*time.Second, 50*time.Millisecond)
	pub, err := app.HostGraph(second).Runtime.MerchantSecretBackend.SolanaTransit.PublicKey(t.Context(), transitKey)
	require.NoError(t, err)
	require.Equal(t, want, solanago.PublicKeyFromBytes(pub).String())
}

// A Vault Transit key replaced behind OpenRails' back is still provisioned as a
// new Solana identity, but loudly: an ERROR log names both public keys and the
// signer identity probe fails so the host alerts.
func TestTransitKeyChangeIsProvisionedAndAlerts(t *testing.T) {
	t.Setenv("VAULT_MAX_RETRIES", "0")
	f := newFixture(t)
	fake := vaultfake.New("greenfield-root")
	t.Cleanup(fake.Close)
	slug := "rotate-" + uuid.NewString()[:8]
	old := solanago.PublicKeyFromBytes(fake.PublicKey(transitKey)).String()

	first := f.resilientRuntime(t, resilientBoot{vault: fake, stripe: &stripeCheckoutFake{t: t}, slug: slug})
	client, err := first.Client()
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, ok := checkoutPSP(t, client, "solana"); return ok }, 30*time.Second, 50*time.Millisecond)
	require.NoError(t, probe(t, first, "openrails_solana_signer_identity"))
	require.NoError(t, first.Close(context.Background()))

	fake.Rotate(transitKey)
	rotated := solanago.PublicKeyFromBytes(fake.PublicKey(transitKey)).String()
	logs := logtest.NewGlobal()
	t.Cleanup(logs.Reset)
	second := f.resilientRuntime(t, resilientBoot{vault: fake, stripe: &stripeCheckoutFake{t: t}, slug: slug})
	require.Eventually(t, func() bool { return probe(t, second, "openrails_solana_signer_identity") != nil }, 30*time.Second, 50*time.Millisecond)
	require.ErrorContains(t, probe(t, second, "openrails_solana_signer_identity"), rotated)
	waitReady(t, second)

	var logged bool
	for _, e := range logs.AllEntries() {
		if e.Level == logrus.ErrorLevel && e.Data["stored_public_key"] == old && e.Data["transit_public_key"] == rotated && e.Data["key"] == transitKey {
			logged = true
		}
	}
	require.True(t, logged, "the key change is logged at ERROR with both public keys")

	require.Eventually(t, func() bool {
		var n int
		err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{f.schema, "psps"}.Sanitize()+" WHERE rail = 'solana' AND account_id = $1", rotated).Scan(&n)
		return err == nil && n == 1
	}, 30*time.Second, 50*time.Millisecond, "the new key is still provisioned")
}
