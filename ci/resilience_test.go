//go:build e2e && integration

package ci_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/solanafake"
	"github.com/open-rails/openrails/internal/vaultfake"
)

const transitKey = "e2e-solana"

// resilienceTreasury is the merchant's Solana treasury.
var resilienceTreasury = solanago.NewWallet().PublicKey()

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

// historylessStripe is a Stripe account with no history. The runtime's
// provider refresh, which runs whenever River schedules it (before or after an
// outage clears), reads empty lists; anything else is the wrapped fake's.
type historylessStripe struct{ http.RoundTripper }

func (s historylessStripe) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet && strings.Count(r.URL.Path, "/") == 2 && r.URL.Path != "/v1/account" && r.URL.Path != "/v1/balance" {
		return jsonResponse(`{"object":"list","data":[],"has_more":false,"url":"` + r.URL.Path + `"}`), nil
	}
	return s.RoundTripper.RoundTrip(r)
}

type resilientBoot struct {
	vault  *vaultfake.Server
	nmi    http.RoundTripper
	stripe http.RoundTripper
	slug   string
	// redisDown hands the runtime an unreachable Redis.
	redisDown bool
	// chain is the devnet node the rail reads; nil starts one.
	chain *solanafake.Node
}

// resilientRuntime is a started engine and its merchant's handle.
func (f *fixture) resilientRuntime(t *testing.T, b resilientBoot) (*openrails.Engine, *openrails.Client) {
	t.Helper()
	var rdb redis.UniversalClient
	if b.redisDown {
		rdb = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
		t.Cleanup(func() { _ = rdb.Close() })
	}
	psps := map[string]openrails.PSPConfig{
		"stripe": openrails.StripePSP{Environment: openrails.EnvironmentTest, AccountID: "acct_e2e", SecretKey: "sk_test_e2e", WebhookSigningSecret: "whsec_e2e"}.PSPConfig(),
		"solana": openrails.SolanaPSP{Environment: openrails.EnvironmentDevnet, Treasury: resilienceTreasury.String()}.PSPConfig(),
	}
	if b.nmi != nil {
		psps["nmi"] = openrails.NMIPSP{Environment: openrails.EnvironmentSandbox, GatewayID: "e2e-nmi", SecurityKey: "e2e-nmi-key", WebhookSigningSecret: "whsec_nmi", TokenizationKey: "e2e-tokenization"}.PSPConfig()
	}
	cfg := f.config()
	cfg.ProviderWriteMode = openrails.ProviderWritesFull
	cfg.Vault = &openrails.VaultConfig{Address: b.vault.URL(), Token: b.vault.Token}
	cfg.Solana = &openrails.SolanaConfig{Crankers: []openrails.SolanaCrankerConfig{{Mode: "vault_transit", Key: transitKey}}}
	if b.chain == nil {
		b.chain = solanafake.New()
		t.Cleanup(b.chain.Close)
	}
	cfg.ProviderSandbox = &openrails.ProviderSandboxConfig{SolanaRPCURL: b.chain.URL()}
	cfg.Merchants = map[string]openrails.MerchantDeclaration{b.slug: {Sandbox: true, DisplayName: b.slug, PSPs: psps}}
	cfg = ownIssuer(cfg)
	start := time.Now()
	rt, err := openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool, Redis: rdb, StripeTransport: historylessStripe{b.stripe}, NMITransport: b.nmi})
	require.NoError(t, err)
	require.Less(t, time.Since(start), 20*time.Second, "construction never waits on an optional provider")
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	require.NoError(t, rt.Start(t.Context()))
	return rt, declared(t, rt, cfg)
}

func probe(t *testing.T, rt *openrails.Engine, name string) error {
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

func routedPSP(t *testing.T, rt *openrails.Engine, rail string) (billing.PSPPaymentConfig, bool) {
	t.Helper()
	for _, psp := range customerConfig(t, rt).Payment.PSPs {
		if psp.Rail == rail {
			return psp, true
		}
	}
	return billing.PSPPaymentConfig{}, false
}

func sign(t *testing.T, rt *openrails.Engine) ([]byte, error) {
	t.Helper()
	return engine.Graph(rt).Runtime.Vault.SolanaTransit.Sign(t.Context(), transitKey, []byte("e2e"))
}

// solanaSession sells a new one-time price, or a recurring one on the plan
// the cranker publishes paying the treasury, while the Solana rail is armed.
func solanaSession(t *testing.T, rt *openrails.Engine, client *openrails.Client, recurring bool) *purchase {
	t.Helper()
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "sol-" + uuid.NewString()[:8], DisplayName: "Solana", Entitlements: []string{"content:sol"}})
	require.NoError(t, err)
	params := billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 1_000_000, Currency: "USD", SellVia: []string{billing.SellViaSolana}}
	if recurring {
		hours := 720
		params.AccessDurationHours, params.BillingIntervalHours = &hours, &hours
	}
	price, err := client.CreatePrice(t.Context(), params)
	require.NoError(t, err)
	require.Equal(t, recurring, price.SolanaPlan != nil, "a recurring price sells on its published plan")
	session, err := sell(t, rt, client, sale{customer: cid(uuid.NewString()), price: price.ID})
	require.NoError(t, err)
	return session
}

// solanaPayStatus pays session's Solana option on rt's payment page and
// returns the HTTP status of the refusal (0 on success).
func solanaPayStatus(t *testing.T, rt *openrails.Engine, session *purchase) int {
	t.Helper()
	_, err := session.on(rt).pay("pay-"+uuid.NewString(), map[string]any{"token_symbol": "USDC"})
	var status *billing.StatusError
	if err == nil {
		return 0
	}
	require.ErrorAs(t, err, &status, err.Error())
	return status.Status
}

func waitReady(t *testing.T, rt *openrails.Engine) {
	t.Helper()
	require.Eventually(t, func() bool { return rt.Ready(t.Context()) == nil }, 10*time.Second, 20*time.Millisecond)
}

func stripeCheckout(t *testing.T, rt *openrails.Engine, client *openrails.Client) {
	t.Helper()
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "post-" + uuid.NewString()[:8], DisplayName: "Post", Entitlements: []string{"content:post"}})
	require.NoError(t, err)
	price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)
	session, err := sell(t, rt, client, sale{customer: cid(uuid.NewString()), price: price.ID})
	require.NoError(t, err)
	paid, err := session.pay("pay-1", redirect("https://e2e.test/success"))
	require.NoError(t, err)
	require.NotNil(t, paid.Payment.NextAction, "%+v", paid)
	require.Equal(t, "redirect_to_url", paid.Payment.NextAction.Type)
}

// First boot with Vault, Redis and the NMI posture probe all unavailable: the
// runtime builds and is ready, the card rails and the Solana PSP (its treasury
// needs no Vault) are offered, and only the deployment's Transit cranker
// waits; each recovers in the background.
func TestOptionalProvidersNeverBlockBoot(t *testing.T) {
	t.Setenv("VAULT_MAX_RETRIES", "0")
	f := newFixture(t)
	fake := vaultfake.New("e2e-root")
	t.Cleanup(fake.Close)
	fake.SetUp(false)
	nmi := heldNMI{release: make(chan struct{})}
	released := false
	t.Cleanup(func() {
		if !released {
			close(nmi.release)
		}
	})

	rt, _ := f.resilientRuntime(t, resilientBoot{vault: fake, nmi: nmi, stripe: &stripeCheckoutFake{t: t}, slug: "resilient-" + uuid.NewString()[:8], redisDown: true})
	waitReady(t, rt)
	require.Eventually(t, func() bool {
		deps, err := engine.Graph(rt).Runtime.Ready(t.Context())
		if err != nil {
			return false
		}
		for _, dep := range deps {
			if dep.Name == "redis" {
				return dep.Optional && !dep.Available && dep.Err != nil
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "unreachable Redis is reported without failing readiness")
	require.ErrorIs(t, probe(t, rt, "openrails_vault"), vault.ErrUnavailable)
	require.Error(t, probe(t, rt, "openrails_psp_posture"), "the NMI verdict is still unknown")

	for _, rail := range []string{"stripe", "nmi", "solana"} {
		_, offered := routedPSP(t, rt, rail)
		require.True(t, offered, rail)
	}
	_, err := sign(t, rt)
	require.ErrorIs(t, err, vault.ErrUnavailable, "the Transit cranker waits for Vault")
	require.NoError(t, probe(t, rt, "openrails_configuration"), "nothing is refused: a Solana PSP needs no Vault")

	fake.SetUp(true)
	require.Eventually(t, func() bool { return probe(t, rt, "openrails_vault") == nil }, 30*time.Second, 50*time.Millisecond)
	signature, err := sign(t, rt)
	require.NoError(t, err)
	require.Len(t, signature, 64)

	released = true
	close(nmi.release)
	require.Eventually(t, func() bool { return probe(t, rt, "openrails_psp_posture") == nil }, 30*time.Second, 50*time.Millisecond, "posture verifies in the background")
	require.NoError(t, rt.Ready(t.Context()))
}

// A restart while Vault is down keeps the Solana PSP, whose treasury is its
// identity, and sells through Stripe; the cranker signs once Vault returns,
// with the same key.
func TestSolanaTreasuryServesWhileVaultIsDown(t *testing.T) {
	t.Setenv("VAULT_MAX_RETRIES", "0")
	f := newFixture(t)
	fake := vaultfake.New("e2e-root")
	t.Cleanup(fake.Close)
	slug := "restart-" + uuid.NewString()[:8]
	want := solanago.PublicKeyFromBytes(fake.PublicKey(transitKey)).String()

	first, _ := f.resilientRuntime(t, resilientBoot{vault: fake, stripe: &stripeCheckoutFake{t: t}, slug: slug})
	require.Eventually(t, func() bool { _, ok := routedPSP(t, first, "solana"); return ok }, 30*time.Second, 50*time.Millisecond)
	stored, _ := routedPSP(t, first, "solana")
	require.NoError(t, first.Close(context.Background()))

	fake.SetUp(false)
	second, client := f.resilientRuntime(t, resilientBoot{vault: fake, stripe: &stripeCheckoutFake{t: t}, slug: slug})
	waitReady(t, second)
	again, ok := routedPSP(t, second, "solana")
	require.True(t, ok, "the treasury needs no Vault")
	require.Equal(t, stored.PSPID, again.PSPID)
	_, err := sign(t, second)
	require.ErrorIs(t, err, vault.ErrUnavailable)
	stripeCheckout(t, second, client)

	fake.SetUp(true)
	require.Eventually(t, func() bool { _, err := sign(t, second); return err == nil }, 30*time.Second, 50*time.Millisecond)
	pub, err := engine.Graph(second).Runtime.Vault.SolanaTransit.PublicKey(t.Context(), transitKey)
	require.NoError(t, err)
	require.Equal(t, want, solanago.PublicKeyFromBytes(pub).String())
}

// A Transit cranker is pinned to the key it first had: rotated behind
// OpenRails' back, it is refused instead of silently becoming another cranker.
// Money never moves elsewhere: the Solana PSP is its treasury and keeps its
// id, one-time payments still pay it, and a recurring price on the cranker's
// plan is refused by name while the probe warns.
func TestRotatedTransitCrankerIsRefused(t *testing.T) {
	t.Setenv("VAULT_MAX_RETRIES", "0")
	f := newFixture(t)
	fake := vaultfake.New("e2e-root")
	t.Cleanup(fake.Close)
	slug := "rotate-" + uuid.NewString()[:8]
	chain := solanafake.New()
	t.Cleanup(chain.Close)
	boot := func() (*openrails.Engine, *openrails.Client) {
		return f.resilientRuntime(t, resilientBoot{vault: fake, stripe: &stripeCheckoutFake{t: t}, slug: slug, chain: chain})
	}

	first, client := boot()
	require.Eventually(t, func() bool { _, ok := routedPSP(t, first, "solana"); return ok }, 30*time.Second, 50*time.Millisecond)
	stored, _ := routedPSP(t, first, "solana")
	monthly := solanaSession(t, first, client, true)
	require.Eventually(t, func() bool { return probe(t, first, "openrails_solana_crankers") == nil }, 30*time.Second, 50*time.Millisecond, "the cranker pins its key")
	require.NoError(t, first.Close(context.Background()))

	fake.Rotate(transitKey)
	second, client := boot()
	waitReady(t, second)
	again, ok := routedPSP(t, second, "solana")
	require.True(t, ok)
	require.Equal(t, stored.PSPID, again.PSPID, "the PSP is its treasury, not its cranker")

	require.Equal(t, http.StatusBadRequest, solanaPayStatus(t, second, monthly), "the rotated cranker signs nothing")
	require.ErrorContains(t, probe(t, second, "openrails_solana_crankers"), "the key it was pinned to")
	require.ErrorContains(t, probe(t, second, "openrails_configuration"), "solana.crankers.vault_transit:"+transitKey+": solana cranker", "the rotated cranker is not served")
	once := solanaSession(t, second, client, false)
	require.Zero(t, solanaPayStatus(t, second, once), "a one-time Solana payment still sells")
}
