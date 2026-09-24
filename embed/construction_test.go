package embed

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Every refusal below happens before bootstrap: the configs carry no database,
// so reaching it would fail with a different error.
func TestNewRefusesInvalidOptionsBeforeOpeningResources(t *testing.T) {
	sandbox := func() *config.Config {
		return &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeReadOnly}
	}
	live := func() *config.Config {
		return &config.Config{TestMode: config.CredentialPostureLive, ProviderWriteMode: config.ProviderWriteModeFull}
	}
	var seam http.RoundTripper = http.DefaultTransport
	for name, tc := range map[string]struct {
		opts Options
		want string
	}{
		"no config":                   {Options{}, "config is required"},
		"merchant without slug":       {Options{Config: sandbox(), Merchant: &MerchantDeclaration{Slug: " "}}, "Merchant.Slug"},
		"psp without key":             {Options{Config: sandbox(), Merchant: &MerchantDeclaration{Slug: "m", PSPs: []PSPDeclaration{{Rail: "stripe", AccountID: "acct"}}}}, "Merchant.PSPs[0]"},
		"psp without rail":            {Options{Config: sandbox(), Merchant: &MerchantDeclaration{Slug: "m", PSPs: []PSPDeclaration{{Key: "k", AccountID: "acct"}}}}, "Merchant.PSPs[0]"},
		"psp without account":         {Options{Config: sandbox(), Merchant: &MerchantDeclaration{Slug: "m", PSPs: []PSPDeclaration{{Key: "k", Rail: "stripe"}}}}, "Merchant.PSPs[0]"},
		"checkout without auth":       {Options{Config: sandbox(), HTTP: &HTTPConfig{Checkout: true}}, "Checkout requires"},
		"customer without verifier":   {Options{Config: sandbox(), HTTP: &HTTPConfig{CustomerRoutes: []CustomerRoutesConfig{{Treasury: true}}}}, "requires its own authenticator"},
		"merchant admin without auth": {Options{Config: sandbox(), HTTP: &HTTPConfig{MerchantAdmin: true}}, "management surfaces require"},
		"catalog without auth":        {Options{Config: sandbox(), HTTP: &HTTPConfig{Catalog: true}}, "management surfaces require"},
		"config without auth":         {Options{Config: sandbox(), HTTP: &HTTPConfig{MerchantConfig: true}}, "management surfaces require"},
		"merchant api without auth":   {Options{Config: sandbox(), HTTP: &HTTPConfig{MerchantAPI: true}}, "management surfaces require"},
		"host river runs workers":     {Options{Config: sandbox(), River: RiverFromHost(), RunWorkers: true}, "managed-only"},
		"river schema injection":      {Options{Config: sandbox(), River: RiverManagedByOpenRails("jobs;drop")}, "River schema"},
		"two river schemas":           {Options{Config: sandbox(), River: RiverManagedByOpenRails("a", "b")}, "at most one"},
		"posture unset (default)":     {Options{Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}}, "config.TestMode is required"},
		"posture unset (host river)":  {Options{Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}, River: RiverFromHost()}, "config.TestMode is required"},
		"unknown write mode":          {Options{Config: &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: "sometimes"}}, "is invalid"},
		"stripe seam on live":         {Options{Config: live(), StripeTransport: seam}, "StripeTransport is a test seam"},
		"nmi seam on live":            {Options{Config: live(), NMITransport: seam}, "NMITransport is a test seam"},
		"clock seam on live":          {Options{Config: live(), Clock: clockwork.NewFakeClock()}, "Clock is a test seam"},
	} {
		t.Run(name, func(t *testing.T) {
			rt, err := New(context.Background(), tc.opts)
			require.Nil(t, rt)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestEmbeddedDefaults(t *testing.T) {
	defaults := config.GetDefaultBillingConfig()
	for _, posture := range []config.CredentialPosture{config.CredentialPostureSandbox, config.CredentialPostureLive} {
		cfg := &config.Config{TestMode: posture, ProviderWriteMode: " Full "}
		require.NoError(t, applyEmbeddedDefaults(cfg))
		require.Equal(t, defaults.RateLimits, cfg.RateLimits, "an embedded surface never ships unthrottled")
		require.Equal(t, config.CaptchaProviderTurnstile, cfg.Captcha.EffectiveProvider())
	}

	custom := &config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}}
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeLimited, RateLimits: custom}
	require.NoError(t, applyEmbeddedDefaults(cfg))
	require.Same(t, custom, cfg.RateLimits)

	cfg = &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeReadOnly, RateLimitsDisabled: true}
	require.NoError(t, applyEmbeddedDefaults(cfg))
	require.Nil(t, cfg.RateLimits, "an explicit opt-out stays a passthrough")
	require.Nil(t, cfg.Captcha)
}

func TestUninitializedRuntimeFailsClosed(t *testing.T) {
	var rt *Runtime
	_, err := rt.HTTPRoutes()
	require.ErrorContains(t, err, "not initialized")
	require.ErrorContains(t, rt.configureHTTP(HTTPConfig{}), "not initialized")
	require.Error(t, rt.RunWorkers(context.Background()))
	require.Error(t, rt.Ready(context.Background()))
	_, err = rt.CheckJobProgress(context.Background())
	require.ErrorIs(t, err, ErrNotInitialized)
	require.False(t, rt.HasExternalRiverClient())
	require.NoError(t, rt.Close(context.Background()))
	_, err = NewHostTransactions(rt).GetOperationAuthorization(context.Background(), nil, "op")
	require.ErrorContains(t, err, "not initialized")
}

// Host transactions run under the runtime's merchant; a caller context pinned
// to another merchant is a conflict, never a re-scope.
func TestHostTransactionsBindRuntimeMerchant(t *testing.T) {
	appRuntime := &app.Runtime{Config: &config.Config{}}
	rt := &Runtime{app: &app.App{Runtime: appRuntime}, svc: &service.Service{}}
	host := NewHostTransactions(rt)
	_, err := host.bind(context.Background())
	require.ErrorContains(t, err, "no merchant is bound")

	bound, other := merchant.ID(uuid.New()), merchant.ID(uuid.New())
	appRuntime.SetConfiguredMerchant(bound)
	ctx, err := host.bind(context.Background())
	require.NoError(t, err)
	got, ok := merchant.FromContext(ctx)
	require.True(t, ok)
	require.Equal(t, bound, got)

	_, err = host.bind(merchant.WithID(context.Background(), bound))
	require.NoError(t, err)
	_, err = host.bind(merchant.WithID(context.Background(), other))
	require.ErrorIs(t, err, openrails.ErrConflict)
}
