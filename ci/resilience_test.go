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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/hosttools"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/signeridentity"
	"github.com/open-rails/openrails/internal/solanafake"
	"github.com/open-rails/openrails/internal/vaultfake"
)

const transitKey = "e2e-solana"

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
	// chain is the Solana node the rail reads; nil is an unreachable one.
	chain *solanafake.Node
}

func (f *fixture) resilientRuntime(t *testing.T, b resilientBoot) *openrails.Client {
	t.Helper()
	var rdb *redis.Client
	if b.redisDown {
		rdb = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
		t.Cleanup(func() { _ = rdb.Close() })
	}
	psps := map[string]openrails.PSPConfig{
		"stripe": openrails.StripePSP{AccountID: "acct_e2e", SecretKey: "sk_test_e2e", WebhookSigningSecret: "whsec_e2e"}.PSPConfig(),
		"solana": openrails.SolanaPSP{TransitKey: transitKey}.PSPConfig(),
	}
	if b.nmi != nil {
		psps["nmi"] = openrails.NMIPSP{AccountID: "e2e-nmi", SecurityKey: "e2e-nmi-key", WebhookSigningSecret: "whsec_nmi", TokenizationKey: "e2e-tokenization"}.PSPConfig()
	}
	cfg := f.config()
	cfg.ProviderWriteMode = openrails.ProviderWritesFull
	cfg.Vault = &openrails.VaultConfig{Address: b.vault.URL(), Token: b.vault.Token}
	cfg.ProviderSandbox = &openrails.ProviderSandboxConfig{SolanaRPCURL: "http://127.0.0.1:1"}
	if b.chain != nil {
		cfg.ProviderSandbox.SolanaRPCURL = b.chain.URL()
		psps["solana"] = openrails.SolanaPSP{TransitKey: transitKey, RPCProvider: "public", Tokens: map[string]openrails.SolanaToken{"SOL": {}, "DUSD": {}}}.PSPConfig()
	}
	cfg.Merchant = openrails.MerchantDeclaration{Slug: b.slug, DisplayName: b.slug, PSPs: psps}
	start := time.Now()
	rt, err := openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool, Redis: rdb, StripeTransport: historylessStripe{b.stripe}, NMITransport: b.nmi})
	require.NoError(t, err)
	require.Less(t, time.Since(start), 20*time.Second, "construction never waits on an optional provider")
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	require.NoError(t, rt.Start(t.Context()))
	return rt
}

func probe(t *testing.T, rt *openrails.Client, name string) error {
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

func checkoutPSP(t *testing.T, client *openrails.Client, rail string) (billing.PSPPaymentConfig, bool) {
	t.Helper()
	cfg := publicConfig(t, client)
	for _, psp := range cfg.Payment.PSPs {
		if psp.Rail == rail {
			return psp, true
		}
	}
	return billing.PSPPaymentConfig{}, false
}

func sign(t *testing.T, rt *openrails.Client) ([]byte, error) {
	t.Helper()
	return engine.Graph(rt).Runtime.Vault.SolanaTransit.Sign(t.Context(), transitKey, []byte("e2e"))
}

// solanaSession sells a new one-time price, or a recurring one on the
// merchant's plan published on chain, while the Solana rail is armed.
func solanaSession(t *testing.T, client *openrails.Client, chain *solanafake.Node, signer solanago.PublicKey) *checkoutSession {
	t.Helper()
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "sol-" + uuid.NewString()[:8], DisplayName: "Solana", Entitlements: []string{"content:sol"}})
	require.NoError(t, err)
	params := billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 1_000_000, Currency: "USD", PSPs: []string{"solana"}}
	if chain != nil {
		hours := 720
		plan, err := chain.Plan(signer, 1, solanafake.DevnetDUSDMint, 1_000_000, uint64(hours))
		require.NoError(t, err)
		params.AccessDurationHours, params.BillingIntervalHours = &hours, &hours
		params.PSPLinks = map[string]map[string]string{"solana": {"plan_pda": plan.String(), "plan_id": "1"}}
	}
	price, err := client.CreatePrice(t.Context(), params)
	require.NoError(t, err)
	session, err := sell(t, client, billing.CreateCheckoutSessionParams{
		Customer: billing.CheckoutCustomerIdentity{ID: cid(uuid.NewString()), VerifiedEmail: "reader@example.test"}, ProductKey: product.Key, PriceKey: price.Key, SuccessURL: "https://e2e.test/success",
	})
	require.NoError(t, err)
	return session
}

// solanaPayStatus pays session's Solana option on client's payment page and
// returns the HTTP status of the refusal (0 on success).
func solanaPayStatus(t *testing.T, client *openrails.Client, session *checkoutSession) int {
	t.Helper()
	_, err := session.on(client).pay("solana", nil)
	var status *billing.StatusError
	if err == nil {
		return 0
	}
	require.ErrorAs(t, err, &status, err.Error())
	return status.Status
}

func waitReady(t *testing.T, rt *openrails.Client) {
	t.Helper()
	require.Eventually(t, func() bool { return rt.Ready(t.Context()) == nil }, 10*time.Second, 20*time.Millisecond)
}

func stripeCheckout(t *testing.T, client *openrails.Client) {
	t.Helper()
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "post-" + uuid.NewString()[:8], DisplayName: "Post", Entitlements: []string{"content:post"}})
	require.NoError(t, err)
	price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)
	session, err := sell(t, client, billing.CreateCheckoutSessionParams{
		Customer: billing.CheckoutCustomerIdentity{ID: cid(uuid.NewString()), VerifiedEmail: "reader@example.test"}, ProductKey: product.Key, PriceKey: price.Key, SuccessURL: "https://e2e.test/success",
	})
	require.NoError(t, err)
	paid, err := session.pay("stripe", nil)
	require.NoError(t, err)
	require.NotNil(t, paid.NextAction, "%+v", paid)
	require.Equal(t, "redirect_to_url", paid.NextAction.Type)
}

// First boot with Vault, Redis and the NMI posture probe all unavailable: the
// runtime builds and is ready, the card rails are offered, only the Solana
// rail is missing and its signer answers unavailable; each recovers in the
// background.
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

	rt := f.resilientRuntime(t, resilientBoot{vault: fake, nmi: nmi, stripe: &stripeCheckoutFake{t: t}, slug: "resilient-" + uuid.NewString()[:8], redisDown: true})
	client := rt
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

	for _, rail := range []string{"stripe", "nmi"} {
		_, offered := checkoutPSP(t, client, rail)
		require.True(t, offered, rail)
	}
	_, armed := checkoutPSP(t, client, "solana")
	require.False(t, armed, "a first boot without Vault leaves only the Solana rail disarmed")
	_, err := sign(t, rt)
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
	fake := vaultfake.New("e2e-root")
	t.Cleanup(fake.Close)
	slug := "restart-" + uuid.NewString()[:8]
	want := solanago.PublicKeyFromBytes(fake.PublicKey(transitKey)).String()

	first := f.resilientRuntime(t, resilientBoot{vault: fake, stripe: &stripeCheckoutFake{t: t}, slug: slug})
	client := first
	require.Eventually(t, func() bool { _, ok := checkoutPSP(t, client, "solana"); return ok }, 30*time.Second, 50*time.Millisecond)
	stored, _ := checkoutPSP(t, client, "solana")
	require.NoError(t, first.Close(context.Background()))

	fake.SetUp(false)
	second := f.resilientRuntime(t, resilientBoot{vault: fake, stripe: &stripeCheckoutFake{t: t}, slug: slug})
	client = second
	waitReady(t, second)
	again, ok := checkoutPSP(t, client, "solana")
	require.True(t, ok, "the stored identity keeps the rail provisioned")
	require.Equal(t, stored.PSPID, again.PSPID)
	_, err := sign(t, second)
	require.ErrorIs(t, err, vault.ErrUnavailable)
	stripeCheckout(t, client)

	fake.SetUp(true)
	require.Eventually(t, func() bool { _, err := sign(t, second); return err == nil }, 30*time.Second, 50*time.Millisecond)
	pub, err := engine.Graph(second).Runtime.Vault.SolanaTransit.PublicKey(t.Context(), transitKey)
	require.NoError(t, err)
	require.Equal(t, want, solanago.PublicKeyFromBytes(pub).String())
}

// A Vault Transit key replaced behind OpenRails' back (or a wrong Vault
// address, namespace or mount) fails closed: the ERROR log names both public
// keys, the signer identity probe fails, and the Solana rail answers
// unavailable until an operator approves the new identity. Approval is
// durable across restarts.
func TestTransitKeyChangeFailsClosedUntilApproved(t *testing.T) {
	t.Setenv("VAULT_MAX_RETRIES", "0")
	f := newFixture(t)
	fake := vaultfake.New("e2e-root")
	t.Cleanup(fake.Close)
	slug := "rotate-" + uuid.NewString()[:8]
	chain := solanafake.New()
	t.Cleanup(chain.Close)
	boot := func() (*openrails.Client, *openrails.Client) {
		rt := f.resilientRuntime(t, resilientBoot{vault: fake, stripe: &stripeCheckoutFake{t: t}, slug: slug, chain: chain})
		client := rt
		return rt, client
	}
	solanaRows := func(account string) (active, archived int) {
		rows, err := f.pool.Query(t.Context(), "SELECT superseded_at IS NOT NULL FROM "+pgx.Identifier{f.schema, "psps"}.Sanitize()+" WHERE rail = 'solana' AND account_id = $1", account)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var a bool
			require.NoError(t, rows.Scan(&a))
			if a {
				archived++
			} else {
				active++
			}
		}
		return active, archived
	}
	railConfig := func(rt *openrails.Client, mid billing.MerchantID) error {
		_, err := engine.Graph(rt).Runtime.RailConfigs.RailConfig(merchant.WithID(t.Context(), mid), "solana", "")
		return err
	}
	old := solanago.PublicKeyFromBytes(fake.PublicKey(transitKey)).String()

	first, client := boot()
	require.Eventually(t, func() bool { _, ok := checkoutPSP(t, client, "solana"); return ok }, 30*time.Second, 50*time.Millisecond)
	require.NoError(t, probe(t, first, "openrails_solana_signer_identity"))
	signerKey := solanago.PublicKeyFromBytes(fake.PublicKey(transitKey))
	once, monthly := solanaSession(t, client, nil, signerKey), solanaSession(t, client, chain, signerKey)
	mid, _, err := hosttools.ResolveMerchant(t.Context(), engine.Graph(first), slug)
	require.NoError(t, err)
	require.NoError(t, first.Close(context.Background()))

	fake.Rotate(transitKey)
	rotated := solanago.PublicKeyFromBytes(fake.PublicKey(transitKey)).String()
	logs := logtest.NewGlobal()
	t.Cleanup(logs.Reset)
	second, client := boot()
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
	require.ErrorIs(t, railConfig(second, mid), vault.ErrSignerUnapproved, "the Solana rail answers unavailable (503)")
	// A session minted before the change no longer sells on Solana: the
	// unapproved rail drops out of routing before anything is signed.
	require.Equal(t, http.StatusUnprocessableEntity, solanaPayStatus(t, client, once), "a one-time Solana checkout is refused")
	require.Equal(t, http.StatusUnprocessableEntity, solanaPayStatus(t, client, monthly), "a recurring Solana subscribe is refused")
	signer := func(rt *openrails.Client) solanaint.Signer {
		r := engine.Graph(rt).Runtime
		return recurring.NewSignerFromPSPs(r.Merchants.Secrets(), r.Vault.SolanaTransit, r.DB, 0, config.ExpectedProviderEnvironment(true))
	}
	_, err = signer(second).PublicKey(merchant.WithID(t.Context(), mid), mid)
	require.ErrorIs(t, err, vault.ErrSignerUnapproved, "recurring subscribe and prepare never sign for an unapproved identity")
	psp, ok := checkoutPSP(t, client, "solana")
	require.True(t, ok)
	require.Equal(t, billing.PSPTemporarilyUnavailable, psp.Status, "checkout lists the unapproved rail as temporarily unavailable")
	active, _ := solanaRows(rotated)
	require.Zero(t, active, "an unapproved identity never receives money")

	// Reloading the configuration cannot clear a pending change: only the
	// approval does.
	_, err = engine.Graph(second).Runtime.MerchantConfig.Reload(t.Context(), mid)
	require.NoError(t, err)
	require.ErrorIs(t, railConfig(second, mid), vault.ErrSignerUnapproved, "a pending change survives a reload")

	// A database failure while checking the stored identity fails closed:
	// nothing is provisioned or approved and the rail stays refused.
	broken, err := pgxpool.New(t.Context(), f.dsn(t))
	require.NoError(t, err)
	broken.Close()
	brokenDB, err := db.NewWithPGXPool(broken, f.schema)
	require.NoError(t, err)
	graph := engine.Graph(second).Runtime
	check := &signeridentity.Transit{TransitClient: graph.Vault.SolanaTransit, DB: brokenDB, Directory: graph.Merchants,
		Slug: slug, Environment: config.ExpectedProviderEnvironment(true)}
	_, err = check.PublicKey(t.Context(), transitKey)
	require.ErrorIs(t, err, vault.ErrUnavailable, "an unreadable stored identity never accepts Vault's key")
	_, err = signeridentity.Approve(t.Context(), brokenDB, graph.Merchants, graph.Vault.SolanaTransit, mid, config.ExpectedProviderEnvironment(true), transitKey)
	require.Error(t, err)
	active, _ = solanaRows(rotated)
	require.Zero(t, active)
	require.ErrorIs(t, railConfig(second, mid), vault.ErrSignerUnapproved)

	require.NoError(t, hosttools.ApproveSolanaSigner(t.Context(), engine.Graph(second), mid, transitKey))
	require.NoError(t, probe(t, second, "openrails_solana_signer_identity"))
	require.NoError(t, railConfig(second, mid))
	pub, err := signer(second).PublicKey(merchant.WithID(t.Context(), mid), mid)
	require.NoError(t, err)
	require.Equal(t, rotated, pub.String())
	active, _ = solanaRows(rotated)
	require.Equal(t, 1, active, "the approved identity is provisioned")
	oldActive, oldArchived := solanaRows(old)
	require.Equal(t, [2]int{0, 1}, [2]int{oldActive, oldArchived}, "the previous identity drains")
	require.NoError(t, second.Close(context.Background()))

	third, client := boot()
	require.Eventually(t, func() bool { _, ok := checkoutPSP(t, client, "solana"); return ok }, 30*time.Second, 50*time.Millisecond)
	require.NoError(t, probe(t, third, "openrails_solana_signer_identity"), "approval survives a restart")
	require.NoError(t, railConfig(third, mid))
}
