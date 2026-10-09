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
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/merchant"
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
		"merchant without slug":         {with(sandbox, func(c *config.Config) { c.Merchant.DisplayName = "x" }), config.Deps{}, "Merchant.Slug"},
		"river schema injection":        {with(sandbox, func(c *config.Config) { c.Database.RiverSchema = "jobs;drop" }), config.Deps{}, "RiverSchema"},
		"derived river schema too long": {with(sandbox, func(c *config.Config) { c.Database.Schema = strings.Repeat("b", 42) }), config.Deps{}, "set Config.Database.RiverSchema"},
		"checkout page with fragment":   {with(sandbox, func(c *config.Config) { c.Checkout.PageURL = "https://pay.example/#x" }), config.Deps{}, "Config.Checkout"},
		"posture unset":                 {config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}, config.Deps{}, "Config.TestMode is required"},
		"write mode unset":              {config.Config{TestMode: config.CredentialPostureSandbox}, config.Deps{}, "ProviderWriteMode is required"},
		"unknown write mode":            {with(sandbox, func(c *config.Config) { c.ProviderWriteMode = "sometimes" }), config.Deps{}, "is invalid"},
		"stripe seam on live":           {live, config.Deps{StripeTransport: seam}, "StripeTransport is a test seam"},
		"nmi seam on live":              {live, config.Deps{NMITransport: seam}, "NMITransport is a test seam"},
		"clock seam on live":            {live, config.Deps{Clock: clockwork.NewFakeClock()}, "Clock is a test seam"},
		"catalog without merchant":      {with(sandbox, func(c *config.Config) { c.Catalog = &catalog.Application{SchemaVersion: 1} }), config.Deps{}, "set Config.Merchant"},
		"invalid catalog": {with(sandbox, func(c *config.Config) {
			c.Merchant.Slug = "m"
			c.Catalog = &catalog.Application{}
		}), config.Deps{}, "Config.Catalog: "},
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
		require.NoError(t, validate(cfg, config.Deps{}))
		require.Equal(t, config.DefaultRateLimits(), cfg.RateLimits, "an embedded surface never ships unthrottled")
		require.Equal(t, config.CaptchaProviderTurnstile, config.CaptchaProvider(cfg.Captcha))
	}

	custom := &config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}}
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeLimited, RateLimits: custom}
	require.NoError(t, validate(cfg, config.Deps{}))
	require.Same(t, custom, cfg.RateLimits)

	cfg = &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeReadOnly, RateLimitsDisabled: true}
	require.NoError(t, validate(cfg, config.Deps{}))
	require.Nil(t, cfg.RateLimits, "an explicit opt-out stays a passthrough")
	require.Nil(t, cfg.Captcha)
}

// Host transactions run under the engine's merchant; a caller context pinned
// to another merchant is a conflict, never a re-scope.
func TestHostTransactionsBindEngineMerchant(t *testing.T) {
	appRuntime := &app.Runtime{Config: &config.Config{}}
	e := &Engine{App: &app.App{Runtime: appRuntime}}
	_, err := e.bind(context.Background())
	require.ErrorContains(t, err, "no merchant is bound")

	bound, other := billing.MerchantID(uuid.New()), billing.MerchantID(uuid.New())
	appRuntime.SetConfiguredMerchant(bound)
	ctx, err := e.bind(context.Background())
	require.NoError(t, err)
	got, ok := merchant.FromContext(ctx)
	require.True(t, ok)
	require.Equal(t, bound, got)

	_, err = e.bind(merchant.WithID(context.Background(), bound))
	require.NoError(t, err)
	_, err = e.bind(merchant.WithID(context.Background(), other))
	require.ErrorIs(t, err, billing.ErrConflict)
}

// Startup catalogs are ordinary partial batches, with a private copy retained
// for any background retry. They need no caller-managed identity or revision.
func TestStartupCatalogNeedsOnlyItsContent(t *testing.T) {
	cfg := config.Config{Catalog: &catalog.Application{SchemaVersion: 1,
		Products: map[string]catalog.ApplyProduct{"premium": {DisplayName: catalog.Value("Premium")}},
	}}
	cfg.Merchant.Slug = "merchant"
	copied, err := declaredCatalog(cfg)
	require.NoError(t, err)
	require.Equal(t, cfg.Catalog, copied)
	cfg.Catalog.Products["premium"] = catalog.ApplyProduct{DisplayName: catalog.Value("Changed after construction")}
	require.Equal(t, "Premium", copied.Products["premium"].DisplayName.Value)
}
