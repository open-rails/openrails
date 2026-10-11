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
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/config"
)

// Every refusal below happens before bootstrap: the configs carry no database,
// so reaching it would fail with a different error.
func TestNewRefusesInvalidConfigBeforeOpeningResources(t *testing.T) {
	sandbox := config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeReadOnly}
	live := config.Config{TestMode: config.CredentialPostureLive, ProviderWriteMode: config.ProviderWriteModeFull}
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
		"posture unset":                 {config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}, config.Deps{}, "Config.TestMode is required"},
		"write mode unset":              {config.Config{TestMode: config.CredentialPostureSandbox}, config.Deps{}, "ProviderWriteMode is required"},
		"unknown write mode":            {with(sandbox, func(c *config.Config) { c.ProviderWriteMode = "sometimes" }), config.Deps{}, "is invalid"},
		"stripe seam on live":           {live, config.Deps{StripeTransport: seam}, "StripeTransport is a test seam"},
		"nmi seam on live":              {live, config.Deps{NMITransport: seam}, "NMITransport is a test seam"},
		"clock seam on live":            {live, config.Deps{Clock: clockwork.NewFakeClock()}, "Clock is a test seam"},
		"invalid catalog": {with(sandbox, func(c *config.Config) {
			c.Merchants = map[string]config.MerchantDeclaration{"m": {Catalog: &catalog.Application{}}}
		}), config.Deps{}, `Config.Merchants["m"].Catalog: `},
	} {
		t.Run(name, func(t *testing.T) {
			e, err := New(context.Background(), tc.cfg, tc.deps)
			require.Nil(t, e)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestEmbeddedDefaults(t *testing.T) {
	for _, posture := range []config.CredentialPosture{config.CredentialPostureSandbox, config.CredentialPostureLive} {
		cfg := &config.Config{TestMode: posture, ProviderWriteMode: " Full "}
		_, err := validate(cfg, config.Deps{})
		require.NoError(t, err)
		require.Equal(t, config.DefaultRateLimits(), cfg.RateLimits, "an embedded surface never ships unthrottled")
		require.Equal(t, config.CaptchaProviderTurnstile, config.CaptchaProvider(cfg.Captcha))
	}

	custom := &config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}}
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeLimited, RateLimits: custom}
	_, err := validate(cfg, config.Deps{})
	require.NoError(t, err)
	require.Same(t, custom, cfg.RateLimits)

	cfg = &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeReadOnly, RateLimitsDisabled: true}
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

// Startup catalogs are ordinary partial batches, with a private copy retained
// for any background retry. They need no caller-managed identity or revision.
func TestStartupCatalogNeedsOnlyItsContent(t *testing.T) {
	m := config.MerchantDeclaration{Slug: "merchant", Catalog: &catalog.Application{SchemaVersion: 1,
		Products: map[string]catalog.ApplyProduct{"premium": {DisplayName: catalog.Value("Premium")}},
	}}
	copied, err := declaredCatalog(m)
	require.NoError(t, err)
	require.Equal(t, m.Catalog, copied)
	m.Catalog.Products["premium"] = catalog.ApplyProduct{DisplayName: catalog.Value("Changed after construction")}
	require.Equal(t, "Premium", copied.Products["premium"].DisplayName.Value)
}
