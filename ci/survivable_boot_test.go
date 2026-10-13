//go:build e2e && integration

package ci_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/vaulttest"
)

// stripeTest is a test-mode Stripe PSP the suite's fake checkout serves.
func stripeTest() openrails.PSPConfig {
	return openrails.StripePSP{Environment: openrails.EnvironmentTest, AccountID: "acct_e2e", SecretKey: "sk_test_e2e", WebhookSigningSecret: "whsec_e2e"}.PSPConfig()
}

// survivingEngine boots cfg with the fake Stripe checkout, starts it and
// waits until it is ready.
func survivingEngine(t *testing.T, f *fixture, cfg openrails.Config) *openrails.Engine {
	t.Helper()
	cfg.ProviderWriteMode = openrails.ProviderWritesFull
	start := time.Now()
	bill, err := openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool, StripeTransport: historylessStripe{&stripeCheckoutFake{t: t}}})
	require.NoError(t, err)
	require.Less(t, time.Since(start), 20*time.Second, "nothing optional holds construction")
	t.Cleanup(func() { _ = bill.Close(context.Background()) })
	require.NoError(t, bill.Start(t.Context()))
	waitReady(t, bill)
	return bill
}

// readiness is the optional dependency name Ready reports.
func readiness(t *testing.T, bill *openrails.Engine, name string) app.ReadinessDependency {
	t.Helper()
	deps, err := engine.Graph(bill).Runtime.Ready(t.Context())
	require.NoError(t, err, "an optional dependency never fails readiness")
	for _, dep := range deps {
		if dep.Name == name {
			return dep
		}
	}
	t.Fatalf("Ready reports no %s", name)
	return app.ReadinessDependency{}
}

// notServedFindings are the merchant's open findings for documents it does
// not serve: the console's review queue.
func notServedFindings(t *testing.T, f *fixture, merchant billing.MerchantID) map[string]string {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), "SELECT subject_key, evidence->>'reason' FROM "+pgx.Identifier{f.schema, "reconciliation_findings"}.Sanitize()+
		" WHERE merchant_id = $1 AND finding_type = $2 AND resolved_at IS NULL AND status = 'requires_review'", merchant.UUID(), app.RefusedDocumentFindingType)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var subject, reason string
		require.NoError(t, rows.Scan(&subject, &reason))
		out[subject] = reason
	}
	require.NoError(t, rows.Err())
	return out
}

// The doujins boot of #1265: a Solana PSP signing with Vault Transit on a
// deployment with no Vault. The engine boots and serves; the card PSP sells;
// only the Solana PSP is refused, and Ready, the configuration probe and the
// console's review queue say why.
func TestSolanaTransitSignerWithoutVaultRefusesOnlyThatPSP(t *testing.T) {
	f := newFixture(t)
	slug := "transit-" + uuid.NewString()[:8]
	cfg := f.config()
	cfg.Merchants = map[string]openrails.MerchantDeclaration{slug: {DisplayName: slug, Issuer: authtest.Issuer, PSPs: map[string]openrails.PSPConfig{
		"stripe": stripeTest(),
		"solana": openrails.SolanaPSP{Environment: openrails.EnvironmentSandbox, TransitKey: transitKey}.PSPConfig(),
	}}}
	bill := survivingEngine(t, f, cfg)
	shop := declared(t, bill, cfg)

	const why = "solana signer mode vault_transit requires a Vault connection"
	err := probe(t, bill, "openrails_configuration")
	require.ErrorContains(t, err, "merchants."+slug+".psps/solana: "+why)
	dep := readiness(t, bill, "configuration")
	require.True(t, dep.Optional)
	require.False(t, dep.Available)
	require.ErrorContains(t, dep.Err, why)
	require.Equal(t, map[string]string{"psps/solana": why}, notServedFindings(t, f, shop.MerchantID()))

	_, offered := routedPSP(t, bill, "solana")
	require.False(t, offered, "the refused PSP is never offered")
	_, offered = routedPSP(t, bill, "stripe")
	require.True(t, offered)
	stripeCheckout(t, bill, shop)
	psps, err := shop.ListPSPs(t.Context(), billing.PSPListParams{})
	require.NoError(t, err)
	require.Len(t, psps.Items, 1)
	require.Equal(t, "stripe", psps.Items[0].Key)
}

// A PSP whose secret is missing is served as declared but never armed: the
// posture probe names it, purchase skips it, and the merchant's other PSP
// sells.
func TestPSPWithoutItsSecretIsNotArmedAndTheRestSells(t *testing.T) {
	f := newFixture(t)
	slug := "secretless-" + uuid.NewString()[:8]
	cfg := f.config()
	cfg.Merchants = map[string]openrails.MerchantDeclaration{slug: {DisplayName: slug, Issuer: authtest.Issuer, PSPs: map[string]openrails.PSPConfig{
		"stripe": stripeTest(),
		"nmi":    openrails.NMIPSP{Environment: openrails.EnvironmentSandbox, AccountID: "e2e-secretless", TokenizationKey: "e2e-tokenization"}.PSPConfig(),
	}}}
	bill := survivingEngine(t, f, cfg)
	shop := declared(t, bill, cfg)

	require.Eventually(t, func() bool {
		err := probe(t, bill, "openrails_psp_posture")
		return err != nil && strings.Contains(err.Error(), "psp nmi: credentials unavailable")
	}, 30*time.Second, 50*time.Millisecond, "the posture probe names the PSP without its secret")
	require.NoError(t, probe(t, bill, "openrails_configuration"), "its document is served")
	_, offered := routedPSP(t, bill, "nmi")
	require.False(t, offered, "a PSP without its secret is never offered")
	stripeCheckout(t, bill, shop)
}

// Redis is optional: one that does not answer, or whose configuration does
// not parse, leaves abuse state in memory and the engine serving. Every other
// optional setting that cannot be served is turned off and named, never a
// boot failure.
func TestOptionalSettingsThatCannotBeServedAreTurnedOff(t *testing.T) {
	f := newFixture(t)
	slug := "optional-" + uuid.NewString()[:8]
	base := f.config()
	base.Merchants = map[string]openrails.MerchantDeclaration{slug: {DisplayName: slug, Issuer: authtest.Issuer, PSPs: map[string]openrails.PSPConfig{"stripe": stripeTest()}}}

	unreachable := base
	unreachable.Redis = &openrails.RedisConfig{Addr: "127.0.0.1:1"}
	bill := survivingEngine(t, f, unreachable)
	require.Eventually(t, func() bool {
		dep := readiness(t, bill, "redis")
		return dep.Optional && !dep.Available && dep.Err != nil
	}, 10*time.Second, 20*time.Millisecond, "unreachable Redis is reported without failing readiness")
	stripeCheckout(t, bill, declared(t, bill, base))
	require.NoError(t, bill.Close(t.Context()))

	broken := base
	broken.Redis = &openrails.RedisConfig{Addr: "cache:6379", URL: "redis://cache:6379"}
	broken.Captcha = &openrails.CaptchaConfig{Provider: "turnstile", SecretKey: "half-a-pair"}
	broken.LLM = &openrails.LLMConfig{Provider: "gemini", APIKey: "k"}
	broken.TrustedProxies = []string{"0.0.0.0/0"}
	broken.CatalogReconciliationInterval = "30minutes"
	broken.SMTP = &openrails.SMTPConfig{}
	broken.ReadCache = openrails.ReadCache{"no_such_read": {RefreshAfter: time.Second, MaxStale: time.Minute}}
	bill = survivingEngine(t, f, broken)
	err := probe(t, bill, "openrails_configuration")
	for _, want := range []string{
		"redis: redis.addr and redis.url are exclusive",
		"captcha: captcha requires BOTH site_key and secret_key",
		`llm: invalid llm.provider "gemini"`,
		`trusted_proxies: CIDR "0.0.0.0/0" matches every address`,
		"catalog_reconciliation_interval:",
		"smtp: Config.SMTP.Host is required",
		"read_cache.no_such_read: names an unknown read",
	} {
		require.ErrorContains(t, err, want)
	}
	stripeCheckout(t, bill, declared(t, bill, base))
}

// One merchant's bad declaration refuses that merchant, or that document,
// alone: the others serve fully. A PSP with a misspelled setting is refused
// with its reason; a merchant document the deployment cannot serve refuses
// its PSPs with it; a key that is not a slug is never provisioned.
func TestOneMerchantsBadDeclarationLeavesTheOthersServing(t *testing.T) {
	f := newFixture(t)
	suffix := uuid.NewString()[:8]
	good, typo, custody := "good-"+suffix, "typo-"+suffix, "custody-"+suffix
	misspelled := stripeTest()
	misspelled.AccountID = "acct_e2e_typo"
	misspelled.Settings = map[string]any{"publishable_kye": "pk_test_e2e"}
	platform := stripeTest()
	platform.AccountID = "acct_e2e_custody"
	cfg := f.config()
	cfg.Merchants = map[string]openrails.MerchantDeclaration{
		good:         {DisplayName: "Good", Issuer: authtest.Issuer, PSPs: map[string]openrails.PSPConfig{"stripe": stripeTest()}},
		typo:         {DisplayName: "Typo", PSPs: map[string]openrails.PSPConfig{"stripe": misspelled}},
		custody:      {DisplayName: "Custody", Settings: billing.MerchantSettings{CardCustody: billing.CardCustodyPlatform}, PSPs: map[string]openrails.PSPConfig{"stripe": platform}},
		"Not A Slug": {DisplayName: "Unprovisionable"},
	}
	bill := survivingEngine(t, f, cfg)

	err := probe(t, bill, "openrails_configuration")
	require.ErrorContains(t, err, "merchants.Not A Slug: ")
	require.ErrorContains(t, err, "merchants."+typo+".psps/stripe: ")
	require.ErrorContains(t, err, "publishable_kye")
	require.ErrorContains(t, err, "merchants."+custody+".merchant: ")
	require.ErrorContains(t, err, "merchants."+custody+".psps/stripe: the merchant document is not served")
	require.NotContains(t, err.Error(), "merchants."+good+".")
	_, err = bill.MerchantBySlug("Not A Slug")
	require.ErrorIs(t, err, billing.ErrMerchantUnresolved)

	for slug, want := range map[string]int{typo: 0, custody: 0, good: 1} {
		shop, err := bill.MerchantBySlug(slug)
		require.NoError(t, err, "a merchant whose document is refused is still provisioned")
		psps, err := shop.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, want, slug)
	}
	typoShop, err := bill.MerchantBySlug(typo)
	require.NoError(t, err)
	findings := notServedFindings(t, f, typoShop.MerchantID())
	require.Contains(t, findings, "psps/stripe")
	require.Contains(t, findings["psps/stripe"], "publishable_kye")

	shop, err := bill.MerchantBySlug(good)
	require.NoError(t, err)
	_, offered := routedPSP(t, bill, "stripe")
	require.True(t, offered)
	stripeCheckout(t, bill, shop)
	require.Empty(t, notServedFindings(t, f, shop.MerchantID()))
}

// Vault holding merchant configuration is optional too: down at boot, the
// engine serves without it, merchant configuration answers unavailable and
// the configuration probe says why; once Vault answers, the configuration
// serves with no restart.
func TestVaultHoldingMerchantConfigurationDownAtBootRecovers(t *testing.T) {
	t.Setenv("VAULT_MAX_RETRIES", "0")
	v := vaulttest.New(t)
	f := newFixture(t)
	vaultUp, proxy := flakyProxy(t, v.Addr)
	slug := "vaulted-" + uuid.NewString()[:8]
	cfg := f.config()
	cfg.Vault = &openrails.VaultConfig{Address: proxy, Token: v.Token, KVMount: v.Mount}
	cfg.Merchants = map[string]openrails.MerchantDeclaration{slug: {Issuer: authtest.Issuer}}
	bill := survivingEngine(t, f, cfg)
	shop := declared(t, bill, cfg)

	require.ErrorContains(t, probe(t, bill, "openrails_configuration"), "vault.merchant_configuration: ")
	_, err := shop.GetMerchantConfiguration(t.Context())
	require.Error(t, err, "merchant configuration answers unavailable while Vault is down")

	vaultUp.Store(true)
	require.Eventually(t, func() bool { return probe(t, bill, "openrails_configuration") == nil }, 90*time.Second, 100*time.Millisecond,
		"the configuration serves once Vault answers")
	_, err = shop.GetMerchantConfiguration(t.Context())
	require.NoError(t, err)
	require.NoError(t, probe(t, bill, "openrails_vault"))
}

// What the engine cannot serve without still fails New: an unreachable
// database, live PSPs beside non-live ones, an unstated provider write mode.
func TestUnservableFoundationsStillFailNew(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err := openrails.New(ctx, openrails.Config{ProviderWriteMode: openrails.ProviderWritesReadOnly,
		DB: &openrails.DBConfig{URL: "postgres://openrails:openrails@127.0.0.1:1/openrails?sslmode=disable"}}, openrails.Deps{})
	require.Error(t, err, "an unreachable database fails New once its context ends")

	f := newFixture(t)
	mixed := f.config()
	mixed.Merchants = map[string]openrails.MerchantDeclaration{"mixed-" + uuid.NewString()[:8]: {PSPs: map[string]openrails.PSPConfig{
		"stripe": openrails.StripePSP{Environment: openrails.EnvironmentLive, AccountID: "acct_e2e_live"}.PSPConfig(),
		"nmi":    openrails.NMIPSP{Environment: openrails.EnvironmentSandbox, AccountID: "e2e-mixed"}.PSPConfig(),
	}}}
	_, err = openrails.New(t.Context(), mixed, openrails.Deps{Postgres: f.pool})
	require.ErrorContains(t, err, "a deployment is all live or all non-live")

	unstated := f.config()
	unstated.ProviderWriteMode = ""
	_, err = openrails.New(t.Context(), unstated, openrails.Deps{Postgres: f.pool})
	require.ErrorContains(t, err, "ProviderWriteMode is required")
}

// flakyProxy forwards to target while up is true and drops every connection
// otherwise, as an unreachable server would. It starts down.
func flakyProxy(t *testing.T, target string) (*atomic.Bool, string) {
	t.Helper()
	u, err := url.Parse(target)
	require.NoError(t, err)
	up := &atomic.Bool{}
	forward := httputil.NewSingleHostReverseProxy(u)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if up.Load() {
			forward.ServeHTTP(w, r)
			return
		}
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0)
			}
			_ = conn.Close()
		}
	}))
	t.Cleanup(server.Close)
	return up, server.URL
}
