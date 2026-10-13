package engine

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
)

// Every refusal below happens before bootstrap: the configs carry no database,
// so reaching it would fail with a different error.
func TestNewRefusesInvalidConfigBeforeOpeningResources(t *testing.T) {
	sandbox := config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}
	live := config.Config{ProviderWriteMode: config.ProviderWriteModeFull, Merchants: map[string]config.MerchantDeclaration{"shop": {PSPs: map[string]config.PSPConfig{
		"stripe": {Rail: billing.RailStripe, Environment: config.EnvironmentLive, AccountID: "acct_1"},
	}}}}
	psps := func(environments ...string) func(*config.Config) {
		return func(c *config.Config) {
			declared := map[string]config.PSPConfig{}
			for i, environment := range environments {
				declared[[]string{"mobius", "wallet"}[i]] = config.PSPConfig{Rail: []billing.Rail{billing.RailNMI, billing.RailSolana}[i], Environment: environment, AccountID: "1"}
			}
			c.Merchants = map[string]config.MerchantDeclaration{"shop": {PSPs: declared}}
		}
	}
	with := func(base config.Config, edit func(*config.Config)) config.Config { edit(&base); return base }
	var seam http.RoundTripper = http.DefaultTransport
	for name, tc := range map[string]struct {
		cfg  config.Config
		deps config.Deps
		want string
	}{
		"merchant key not a slug": {with(sandbox, func(c *config.Config) { c.Merchants = map[string]config.MerchantDeclaration{"Not A Slug": {}} }), config.Deps{}, `Config.Merchants["Not A Slug"]`},
		"slug not its key":        {with(sandbox, func(c *config.Config) { c.Merchants = map[string]config.MerchantDeclaration{"a": {Slug: "b"}} }), config.Deps{}, `Slug "b" is not its key`},
		"issuer declared twice": {with(sandbox, func(c *config.Config) {
			c.Merchants = map[string]config.MerchantDeclaration{"a": {Issuer: "https://shop.example"}, "b": {Issuer: "https://shop.example"}}
		}), config.Deps{}, "an issuer binds its credentials to one merchant"},
		"issuer over http": {with(sandbox, func(c *config.Config) {
			c.Merchants = map[string]config.MerchantDeclaration{"a": {Issuer: "http://shop.example"}}
		}), config.Deps{}, "must be https"},
		"issuer with query": {with(sandbox, func(c *config.Config) {
			c.Merchants = map[string]config.MerchantDeclaration{"a": {Issuer: "https://shop.example?x=1"}}
		}), config.Deps{}, "without query or fragment"},
		"issuer not a URL":              {with(sandbox, func(c *config.Config) { c.Merchants = map[string]config.MerchantDeclaration{"a": {Issuer: "shop"}} }), config.Deps{}, "not an absolute URL"},
		"river schema injection":        {with(sandbox, func(c *config.Config) { c.Database.RiverSchema = "jobs;drop" }), config.Deps{}, "RiverSchema"},
		"derived river schema too long": {with(sandbox, func(c *config.Config) { c.Database.Schema = strings.Repeat("b", 42) }), config.Deps{}, "set Config.Database.RiverSchema"},
		"environment undeclared":        {with(sandbox, psps("")), config.Deps{}, "psps.mobius.environment is required"},
		"environment the rail lacks":    {with(sandbox, psps(config.EnvironmentLive, config.EnvironmentTest)), config.Deps{}, `solana has no "test" environment`},
		"live beside non-live":          {with(sandbox, psps(config.EnvironmentLive, config.EnvironmentSandbox)), config.Deps{}, "a deployment is all live or all non-live: live PSPs shop/mobius beside non-live PSPs shop/wallet (sandbox)"},
		"loopback gateway beside live": {with(live, func(c *config.Config) {
			c.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: "http://127.0.0.1:9"}
		}), config.Deps{}, "provider_sandbox.nmi_gateway_url is a test seam and is refused beside a live PSP (shop/stripe)"},
		"write mode unset":    {config.Config{}, config.Deps{}, "ProviderWriteMode is required"},
		"unknown write mode":  {with(sandbox, func(c *config.Config) { c.ProviderWriteMode = "sometimes" }), config.Deps{}, "is invalid"},
		"stripe seam on live": {live, config.Deps{StripeTransport: seam}, "StripeTransport is a test seam"},
		"nmi seam on live":    {live, config.Deps{NMITransport: seam}, "NMITransport is a test seam"},
		"clock seam on live":  {live, config.Deps{Clock: clockwork.NewFakeClock()}, "Clock is a test seam"},
	} {
		t.Run(name, func(t *testing.T) {
			e, err := New(context.Background(), tc.cfg, tc.deps)
			require.Nil(t, e)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestEmbeddedDefaults(t *testing.T) {
	cfg0 := &config.Config{ProviderWriteMode: " Full "}
	_, err := validate(cfg0, config.Deps{})
	require.NoError(t, err)
	require.Equal(t, config.DefaultRateLimits(), cfg0.RateLimits, "an embedded surface never ships unthrottled")
	require.Equal(t, config.CaptchaProviderTurnstile, config.CaptchaProvider(cfg0.Captcha))

	custom := &config.RateLimitsConfig{"purchase": {RequestsPerMinute: 1}}
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeLimited, RateLimits: custom}
	_, err = validate(cfg, config.Deps{})
	require.NoError(t, err)
	require.Same(t, custom, cfg.RateLimits)

	cfg = &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, RateLimitsDisabled: true}
	_, err = validate(cfg, config.Deps{})
	require.NoError(t, err)
	require.Nil(t, cfg.RateLimits, "an explicit opt-out stays a passthrough")
	require.Nil(t, cfg.Captcha)
}

// MerchantBySlug's lookup: only a slug Config declares names its merchant,
// and an engine that declares none names nothing.
func TestDeclaredMerchant(t *testing.T) {
	shop, other := billing.MerchantID(uuid.New()), billing.MerchantID(uuid.New())
	e := &Engine{declared: map[string]billing.MerchantID{"shop": shop, "other": other}}
	for slug, want := range map[string]billing.MerchantID{"shop": shop, " SHOP ": shop, "other": other} {
		got, ok := e.DeclaredMerchant(slug)
		require.True(t, ok, slug)
		require.Equal(t, want, got)
	}
	for _, slug := range []string{"", "third", shop.String()} {
		_, ok := e.DeclaredMerchant(slug)
		require.False(t, ok, slug)
	}
	_, ok := (&Engine{}).DeclaredMerchant("shop")
	require.False(t, ok, "an engine declaring no merchant")
}
