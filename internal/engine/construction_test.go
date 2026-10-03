package engine

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Every refusal below happens before bootstrap: the configs carry no database,
// so reaching it would fail with a different error.
func TestNewRefusesInvalidConfigBeforeOpeningResources(t *testing.T) {
	sandbox := config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeReadOnly}
	live := config.Config{TestMode: config.CredentialPostureLive, ProviderWriteMode: config.ProviderWriteModeFull}
	with := func(base config.Config, edit func(*config.Config)) config.Config { edit(&base); return base }
	authenticate := func(*http.Request) (billingauth.Identity, error) {
		return billingauth.Identity{}, billingauth.ErrUnauthenticated
	}
	var seam http.RoundTripper = http.DefaultTransport
	for name, tc := range map[string]struct {
		cfg  config.Config
		deps config.Deps
		want string
	}{
		"merchant without slug":     {with(sandbox, func(c *config.Config) { c.Merchant.DisplayName = "x" }), config.Deps{}, "Merchant.Slug"},
		"checkout without auth":     {with(sandbox, func(c *config.Config) { c.HTTP = &config.HTTPConfig{Checkout: true} }), config.Deps{}, "Checkout requires"},
		"customer without merchant": {with(sandbox, func(c *config.Config) { c.HTTP = &config.HTTPConfig{CustomerRoutes: []config.CustomerRoutesConfig{{}}} }), config.Deps{Authenticate: authenticate}, "explicit merchant slug"},
		"customer without verifier": {with(sandbox, func(c *config.Config) {
			c.HTTP = &config.HTTPConfig{CustomerRoutes: []config.CustomerRoutesConfig{{Treasury: true}}}
		}), config.Deps{}, "requires its own authenticator"},
		"merchant admin without auth": {with(sandbox, func(c *config.Config) { c.HTTP = &config.HTTPConfig{MerchantAdmin: true} }), config.Deps{}, "management surfaces require"},
		"catalog without authorize":   {with(sandbox, func(c *config.Config) { c.HTTP = &config.HTTPConfig{Catalog: true} }), config.Deps{Authenticate: authenticate}, "management surfaces require"},
		"control plane with groups": {with(sandbox, func(c *config.Config) {
			c.ControlPlane = &config.ControlPlaneConfig{}
			c.HTTP = &config.HTTPConfig{Checkout: true}
		}), config.Deps{}, "may only add CustomerRoutes"},
		"river schema injection":  {with(sandbox, func(c *config.Config) { c.RiverSchema = "jobs;drop" }), config.Deps{}, "RiverSchema"},
		"river schema host owned": {with(sandbox, func(c *config.Config) { c.River = config.RiverHostOwned; c.RiverSchema = "jobs" }), config.Deps{}, "applies to managed River"},
		"unknown river owner":     {with(sandbox, func(c *config.Config) { c.River = "nobody" }), config.Deps{}, "Config.River"},
		"posture unset":           {config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}, config.Deps{}, "Config.TestMode is required"},
		"write mode unset":        {config.Config{TestMode: config.CredentialPostureSandbox}, config.Deps{}, "ProviderWriteMode is required"},
		"unknown write mode":      {with(sandbox, func(c *config.Config) { c.ProviderWriteMode = "sometimes" }), config.Deps{}, "is invalid"},
		"authorize without authn": {sandbox, config.Deps{Authorize: func(*http.Request, billingauth.Identity, billingauth.Requirement) error { return nil }}, "require Deps.Authenticate"},
		"half a user directory":   {sandbox, config.Deps{UserExists: func(context.Context, string) (bool, error) { return true, nil }}, "together"},
		"stripe seam on live":     {live, config.Deps{StripeTransport: seam}, "StripeTransport is a test seam"},
		"nmi seam on live":        {live, config.Deps{NMITransport: seam}, "NMITransport is a test seam"},
		"clock seam on live":      {live, config.Deps{Clock: clockwork.NewFakeClock()}, "Clock is a test seam"},
		"two credential sources": {with(sandbox, func(c *config.Config) {
			c.Merchant = config.MerchantDeclaration{Slug: "m", PSPs: map[string]config.PSPConfig{"stripe": {"stripe": {AccountID: "acct", Secrets: map[string]string{"secret_key": "sk"}}}}}
		}), config.Deps{ProviderCredentials: []config.ProviderCredentialSnapshot{{Rail: "stripe"}}}, "either Deps.ProviderCredentials or Config.Merchant"},
	} {
		t.Run(name, func(t *testing.T) {
			e, err := New(context.Background(), tc.cfg, tc.deps)
			require.Nil(t, e)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestEmbeddedDefaults(t *testing.T) {
	defaults := config.GetDefaultBillingConfig()
	for _, posture := range []config.CredentialPosture{config.CredentialPostureSandbox, config.CredentialPostureLive} {
		cfg := &config.Config{TestMode: posture, ProviderWriteMode: " Full "}
		require.NoError(t, validate(cfg, config.Deps{}))
		require.Equal(t, defaults.RateLimits, cfg.RateLimits, "an embedded surface never ships unthrottled")
		require.Equal(t, config.CaptchaProviderTurnstile, cfg.Captcha.EffectiveProvider())
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

// A native user's own credential is a user session unless the host says
// otherwise; Authorize's sentinels map onto 401 and 403.
func TestAuthenticationHooks(t *testing.T) {
	var identity billingauth.Identity
	var authzErr error
	auth := integration(config.Deps{
		Authenticate: func(*http.Request) (billingauth.Identity, error) { return identity, nil },
		Authorize:    func(*http.Request, billingauth.Identity, billingauth.Requirement) error { return authzErr },
	})
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	for kind, want := range map[billingauth.PrincipalKind]billingauth.CredentialClass{
		billingauth.User: billingauth.CredentialClassUserSession, billingauth.Machine: billingauth.CredentialClassAutomation,
	} {
		identity = billingauth.Identity{Kind: kind}
		got, err := auth.Authentication.AuthenticateRequest(r.Context(), r)
		require.NoError(t, err)
		require.Equal(t, want, got.CredentialClass)
	}
	for err, status := range map[error]int{billingauth.ErrUnauthenticated: http.StatusUnauthorized, billingauth.ErrForbidden: http.StatusForbidden} {
		authzErr = err
		var gate billingauth.GateError
		require.ErrorAs(t, auth.Authorization.Authorize(r.Context(), r, identity, billingauth.Requirement{}), &gate)
		require.Equal(t, status, gate.Status)
	}
	require.Nil(t, integration(config.Deps{}), "no hook authenticates nobody")
}

// Host transactions run under the engine's merchant; a caller context pinned
// to another merchant is a conflict, never a re-scope.
func TestHostTransactionsBindEngineMerchant(t *testing.T) {
	appRuntime := &app.Runtime{Config: &config.Config{}}
	e := &Engine{App: &app.App{Runtime: appRuntime}}
	_, err := e.bind(context.Background())
	require.ErrorContains(t, err, "no merchant is bound")

	bound, other := merchant.ID(uuid.New()), merchant.ID(uuid.New())
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
