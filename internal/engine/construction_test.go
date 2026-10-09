package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/helpers/auth"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/requestauth"
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
		"merchant without slug":         {with(sandbox, func(c *config.Config) { c.Merchant.DisplayName = "x" }), config.Deps{}, "Merchant.Slug"},
		"river schema injection":        {with(sandbox, func(c *config.Config) { c.RiverSchema = "jobs;drop" }), config.Deps{}, "RiverSchema"},
		"derived river schema too long": {with(sandbox, func(c *config.Config) { c.Schema = strings.Repeat("b", 42) }), config.Deps{}, "set Config.RiverSchema"},
		"checkout page with fragment":   {with(sandbox, func(c *config.Config) { c.Checkout.PageURL = "https://pay.example/#x" }), config.Deps{}, "Config.Checkout"},
		"posture unset":                 {config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}, config.Deps{}, "Config.TestMode is required"},
		"write mode unset":              {config.Config{TestMode: config.CredentialPostureSandbox}, config.Deps{}, "ProviderWriteMode is required"},
		"unknown write mode":            {with(sandbox, func(c *config.Config) { c.ProviderWriteMode = "sometimes" }), config.Deps{}, "is invalid"},
		"authorize without authn":       {sandbox, config.Deps{Authorize: func(*http.Request, billingauth.Identity, billingauth.Requirement) error { return nil }}, "require Deps.Authenticate"},
		"authkit and authenticate":      {sandbox, config.Deps{AuthKit: verifier{}, Authenticate: authenticate}, "not both"},
		"customer hook without kit": {sandbox, config.Deps{CustomerFor: func(context.Context, billingauth.Identity) (billing.CustomerID, error) {
			return billing.CustomerID{}, nil
		}}, "require Deps.AuthKit"},
		"nil authkit client":        {sandbox, config.Deps{AuthKit: (*verifier)(nil)}, "Deps.AuthKit"},
		"control plane and authkit": {with(sandbox, func(c *config.Config) { c.ControlPlane = &config.ControlPlaneConfig{} }), config.Deps{AuthKit: verifier{}}, "its own AuthKit"},
		"half a user directory":     {sandbox, config.Deps{UserExists: func(context.Context, string) (bool, error) { return true, nil }}, "together"},
		"stripe seam on live":       {live, config.Deps{StripeTransport: seam}, "StripeTransport is a test seam"},
		"nmi seam on live":          {live, config.Deps{NMITransport: seam}, "NMITransport is a test seam"},
		"clock seam on live":        {live, config.Deps{Clock: clockwork.NewFakeClock()}, "Clock is a test seam"},
		"catalog without merchant":  {with(sandbox, func(c *config.Config) { c.Catalog = &catalog.Application{SchemaVersion: 1} }), config.Deps{}, "set Config.Merchant"},
		"invalid catalog": {with(sandbox, func(c *config.Config) {
			c.Merchant.Slug = "m"
			c.Catalog = &catalog.Application{}
		}), config.Deps{}, "Config.Catalog: "},
		"catalog with control plane": {with(sandbox, func(c *config.Config) {
			c.ControlPlane = &config.ControlPlaneConfig{}
			c.Merchant.Slug = "m"
			c.Catalog = &catalog.Application{SchemaVersion: 1}
		}), config.Deps{}, "control plane's merchants"},
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

// A native user's own credential is a user session unless the host says
// otherwise; Authorize's sentinels map onto 401 and 403.
func TestAuthenticationHooks(t *testing.T) {
	var identity billingauth.Identity
	var authzErr error
	hooks, err := integration(config.Deps{
		Authenticate: func(*http.Request) (billingauth.Identity, error) { return identity, nil },
		Authorize:    func(*http.Request, billingauth.Identity, billingauth.Requirement) error { return authzErr },
	})
	require.NoError(t, err)
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	for kind, want := range map[billingauth.PrincipalKind]billingauth.CredentialClass{
		billingauth.User: billingauth.CredentialClassUserSession, billingauth.Machine: billingauth.CredentialClassAutomation,
	} {
		identity = billingauth.Identity{Kind: kind}
		got, err := hooks.Authentication.AuthenticateRequest(r.Context(), r)
		require.NoError(t, err)
		require.Equal(t, want, got.CredentialClass)
	}
	for err, status := range map[error]int{billingauth.ErrUnauthenticated: http.StatusUnauthorized, billingauth.ErrForbidden: http.StatusForbidden} {
		authzErr = err
		var gate billingauth.GateError
		require.ErrorAs(t, hooks.Authorization.Authorize(r.Context(), r, identity, billingauth.Requirement{}), &gate)
		require.Equal(t, status, gate.Status)
	}
	none, err := integration(config.Deps{})
	require.NoError(t, err)
	require.Nil(t, none, "no hook authenticates nobody")
}

// verifier is a host's AuthKit as OpenRails sees it: it verifies a request
// into a helpers/auth principal whose Can checks permissions live.
type verifier struct {
	identity auth.Identity
	allowed  map[string]bool // "group/permission"
	stepUp   error
}

func (v verifier) AuthenticateRequest(context.Context, *http.Request) (auth.Principal, error) {
	return v, nil
}
func (v verifier) Identity() auth.Identity { return v.identity }
func (v verifier) Can(_ context.Context, scope auth.Scope, permission string) (bool, error) {
	return v.allowed[scope.ID+"/"+permission], nil
}
func (v verifier) CheckRecentSignIn(context.Context) error { return v.stepUp }

// With Deps.AuthKit the host writes no mapping: an AuthKit user pays for
// themselves, staff authority is checked live in the group AuthorityFor
// names, and the recent sign-in check is the principal's own.
func TestAuthKitDerivesAuthentication(t *testing.T) {
	const user, org, issuer = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "https://auth.example"
	kit := verifier{identity: auth.Identity{Kind: auth.KindUser, Issuer: issuer, Subject: user, Email: "u@example.com"},
		allowed: map[string]bool{"staff/merchant:payments:refund": true}, stepUp: auth.ErrStepUpRequired}
	request := func() *http.Request { return requestauth.Begin(httptest.NewRequest(http.MethodGet, "/", nil)) }

	derived, err := integration(config.Deps{AuthKit: kit})
	require.NoError(t, err)
	r := request()
	got, err := derived.Authentication.AuthenticateRequest(r.Context(), r)
	require.NoError(t, err)
	require.Equal(t, billingauth.Identity{Kind: billingauth.User, Issuer: issuer, SubjectID: user, CustomerID: billing.CustomerID(uuid.MustParse(user)), Email: "u@example.com", CredentialClass: billingauth.CredentialClassUserSession}, got)
	require.Nil(t, derived.Authorization, "no AuthorityFor publishes no staff routes")
	require.ErrorIs(t, derived.RecentSignIn.CheckRecentSignIn(r.Context(), r), auth.ErrStepUpRequired)

	mapped, err := integration(config.Deps{AuthKit: kit,
		CustomerFor: func(_ context.Context, caller billingauth.Identity) (billing.CustomerID, error) {
			require.Equal(t, user, caller.SubjectID)
			return billing.CustomerID(uuid.MustParse(org)), nil
		},
		AuthorityFor: func(_ context.Context, required billingauth.Requirement) (billingauth.Authority, error) {
			return billingauth.Authority{Scope: auth.Scope{Authority: issuer, ID: "staff"}, Permission: required.Permission}, nil
		},
	})
	require.NoError(t, err)
	r = request()
	got, err = mapped.Authentication.AuthenticateRequest(r.Context(), r)
	require.NoError(t, err)
	require.Equal(t, org, got.CustomerID.String(), "the hook names who pays")
	require.NoError(t, mapped.Authorization.Authorize(r.Context(), r, got, billingauth.Requirement{Permission: "merchant:payments:refund"}))
	var gate billingauth.GateError
	require.ErrorAs(t, mapped.Authorization.Authorize(r.Context(), r, got, billingauth.Requirement{Permission: "merchant:settings:update"}), &gate)
	require.Equal(t, http.StatusForbidden, gate.Status)
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
		Products: []catalog.ApplyProduct{{Key: "premium", DisplayName: catalog.Value("Premium")}},
	}}
	cfg.Merchant.Slug = "merchant"
	copied, err := declaredCatalog(cfg)
	require.NoError(t, err)
	require.Equal(t, cfg.Catalog, copied)
	cfg.Catalog.Products[0].DisplayName = catalog.Value("Changed after construction")
	require.Equal(t, "Premium", copied.Products[0].DisplayName.Value)
}
