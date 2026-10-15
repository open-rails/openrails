package controlplane

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
)

// The server passes the host's AuthKit configuration through, setting only
// the product's own: roles, the API key prefix, its River schema, the
// resource's scopes, invitation-only local registration, and its HTTP mount.
// Every refusal precedes AuthKit's construction, so none needs a database.
func TestAuthKitConfiguration(t *testing.T) {
	engine := &config.Config{TrustedProxies: []string{"10.0.0.0/8"}}
	issuer := authkit.TokenConfig{Issuer: "https://openrails.example.com"}
	for want, tc := range map[string]struct {
		auth authkit.Config
		opts Options
	}{
		"auth.token.issuer is required":     {authkit.Config{}, Options{}},
		"is not one of open":                {authkit.Config{Token: issuer, Registration: authkit.RegistrationConfig{NativeUserMode: "sometimes"}}, Options{}},
		"need local sign-in":                {authkit.Config{Token: issuer, Registration: authkit.RegistrationConfig{NativeUserMode: iam.RegistrationModeOpen}}, Options{}},
		"open self-registration":            {authkit.Config{Token: issuer, Registration: authkit.RegistrationConfig{NativeUserMode: iam.RegistrationModeOpen}}, Options{LocalSignIn: true}},
		"passwordless login need local sig": {authkit.Config{Token: issuer, Registration: authkit.RegistrationConfig{PasswordlessLogin: true}}, Options{}},
	} {
		_, err := AuthKit(engine, tc.auth, tc.opts)
		require.ErrorContains(t, err, want)
	}

	got, err := AuthKit(engine, authkit.Config{Token: issuer}, Options{})
	require.NoError(t, err)
	require.Same(t, Roles, got.Roles)
	require.Equal(t, APIKeyPrefix, got.APIKeys.Prefix)
	require.Equal(t, []string{billingauth.TokenAudience}, got.Token.IssuedAudiences)
	require.Equal(t, config.RiverSchemaName(engine), got.Database.RiverSchema)
	require.Empty(t, got.Resource.Scopes, "no resource, no scopes")
	require.Equal(t, iam.RegistrationModeClosed, got.Registration.NativeUserMode, "a self-hosted server registers nobody unless it says so")
	require.Equal(t, IntentionalRouteGroups, got.HTTP.Groups, "no resource, nothing for trusted issuers")
	require.Equal(t, []string{"10.0.0.0/8"}, got.HTTP.TrustedProxies, "the engine's proxies")
	require.Equal(t, "/auth", got.HTTP.APIPath)
	require.True(t, got.HTTP.RefreshCookie)

	got, err = AuthKit(engine, authkit.Config{
		Token:        issuer,
		Resource:     authkit.ResourceConfig{ID: "https://api.openrails.example.com", Scopes: map[string][]string{"x": {"y"}}},
		Registration: authkit.RegistrationConfig{NativeUserMode: iam.RegistrationModeInviteOnly},
		HTTP:         &authkit.HTTPConfig{DirectPeerIP: true},
	}, Options{LocalSignIn: true, Console: true})
	require.NoError(t, err)
	require.Equal(t, ResourceScopes, got.Resource.Scopes, "the resource's scopes are the product's")
	require.Equal(t, iam.RegistrationVerificationRequired, got.Registration.Verification)
	require.Contains(t, got.HTTP.Groups, iam.RouteRegistration)
	require.NotContains(t, got.HTTP.Groups, iam.RouteBrowserOIDC)
	require.NotContains(t, got.HTTP.Groups, iam.RouteAdmin)
	require.Equal(t, "/admin/invite", got.Frontend.InvitePath)
	require.Subset(t, got.HTTP.Groups, []iam.RouteGroup{iam.RouteAuthorizationServer, iam.RouteSCIM}, "what trusted issuers call: the token endpoint and the directory")
	require.Empty(t, got.HTTP.TrustedProxies, "a declared posture is kept")

	pool, err := pgxpool.New(t.Context(), "postgres://127.0.0.1:1/unreachable")
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = New(nil, engine, got, pool, Options{})
	require.ErrorContains(t, err, "AuthKit client is required")
}

func TestAuthKitPreservesNoAutomaticMigrations(t *testing.T) {
	for _, engineSkip := range []bool{false, true} {
		for _, authSkip := range []bool{false, true} {
			cfg := &config.Config{Database: config.DatabaseConfig{SkipMigrations: engineSkip}}
			authCfg := authkit.Config{Token: authkit.TokenConfig{Issuer: "https://openrails.example"}, Database: authkit.DatabaseConfig{SkipMigrations: authSkip}}
			got, err := AuthKit(cfg, authCfg, Options{})
			require.NoError(t, err)
			require.Equal(t, engineSkip || authSkip, got.Database.SkipMigrations)
		}
	}
}

func TestLocalConsoleEnrollmentAndLinkDefaults(t *testing.T) {
	engine := &config.Config{PublicBillingBaseURL: "https://billing.example.com/v1"}
	got, err := AuthKit(engine, authkit.Config{Token: authkit.TokenConfig{Issuer: "https://billing.example.com/auth"}}, Options{LocalSignIn: true, Console: true})
	require.NoError(t, err)
	require.Equal(t, iam.RegistrationModeInviteOnly, got.Registration.NativeUserMode)
	require.Equal(t, "https://billing.example.com", got.Frontend.BaseURL)
	require.Equal(t, "/admin/invite", got.Frontend.InvitePath)
	require.Equal(t, "/admin/reset", got.Frontend.PasswordResetPath)
	require.Equal(t, "/admin/verify", got.Frontend.VerifyPath)
	require.Equal(t, "/admin/passwordless", got.Frontend.PasswordlessPath)
	require.NotContains(t, got.HTTP.Groups, iam.RouteAdmin)
}

func TestLocalSignInWithoutConsole(t *testing.T) {
	engine := &config.Config{PublicBillingBaseURL: "https://billing.example.com/v1"}
	for _, frontend := range []authkit.FrontendConfig{{}, {
		BaseURL: "https://accounts.example.com", InvitePath: "/join", PasswordResetPath: "/reset",
		VerifyPath: "/verify", PasswordlessPath: "/sign-in",
	}} {
		got, err := AuthKit(engine, authkit.Config{
			Token: authkit.TokenConfig{Issuer: "https://billing.example.com/auth"}, Frontend: frontend,
		}, Options{LocalSignIn: true})
		require.NoError(t, err)
		require.Equal(t, frontend, got.Frontend, "API-only local sign-in does not assume /admin link landings")
		require.Empty(t, got.AuthorizationServer.Clients, "no implicit console OAuth client")
		require.Equal(t, iam.RegistrationModeInviteOnly, got.Registration.NativeUserMode)
		require.Contains(t, got.HTTP.Groups, iam.RouteRegistration)
		require.Equal(t, ResourceScopes, got.Resource.Scopes, "resource authority is independent of its console")
	}
}

func TestHostedOpenRegistrationRequiresPolicyAndLocalSignIn(t *testing.T) {
	engine := &config.Config{}
	auth := authkit.Config{Token: authkit.TokenConfig{Issuer: "https://billing.example.com/auth"},
		Registration: authkit.RegistrationConfig{NativeUserMode: iam.RegistrationModeOpen}}
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"standalone", Options{LocalSignIn: true}, "open self-registration"},
		{"hosted without sign-in", Options{MerchantCreation: &MerchantCreationConfig{}}, "need local sign-in"},
		{"hosted", Options{LocalSignIn: true, MerchantCreation: &MerchantCreationConfig{}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AuthKit(engine, auth, tc.opts)
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
				return
			}
			require.NoError(t, err)
			require.Equal(t, iam.RegistrationModeOpen, got.Registration.NativeUserMode)
			require.Equal(t, iam.RegistrationVerificationRequired, got.Registration.Verification)
			require.Contains(t, got.HTTP.Groups, iam.RouteRegistration)
		})
	}
}

// AuthKit's routes sit beneath /auth under an origin issuer, else beneath
// the issuer's path, which is AuthKit's base path.
func TestAuthAPIPath(t *testing.T) {
	for issuer, want := range map[string][2]string{
		"https://openrails.example":       {"/auth", "/auth"},
		"https://openrails.example/":      {"/auth", "/auth"},
		"https://api.example/auth":        {"/", "/auth"},
		"https://api.example/identity/v2": {"/", "/identity/v2"},
	} {
		require.Equal(t, want, [2]string{authAPIPath(issuer), authPrefix(issuer)}, issuer)
	}
}

// A missing or partial control plane fails closed with a typed error, never a panic.
func TestUnconfiguredControlPlaneFailsClosed(t *testing.T) {
	ctx := context.Background()
	for _, cp := range []*ControlPlane{nil, {}} {
		require.Equal(t, iam.RegistrationModeClosed, cp.Registration())
		require.Nil(t, cp.AuthHandler())
		require.Empty(t, cp.AuthRoutes())
		require.Empty(t, cp.AuthAPIBase())
		_, err := cp.MerchantScope(ctx, billing.MerchantID{1})
		require.ErrorIs(t, err, ErrNoControlPlane)
		_, _, err = cp.MerchantByName(ctx, "acme")
		require.ErrorIs(t, err, ErrNoControlPlane)
		_, isMerchant, err := cp.MerchantOfScope(ctx, billingauth.Scope{Authority: "x", ID: "y"})
		require.False(t, isMerchant)
		require.NoError(t, err)
		_, err = cp.TrustedIssuers(ctx, billing.MerchantID{1})
		require.ErrorIs(t, err, ErrNoControlPlane)
		cp.Close()
	}
}

// Fleet aggregates refuse an out-of-range window; they never substitute one.
func TestFleetAggregatesRefuseOutOfRangeWindows(t *testing.T) {
	var cp *ControlPlane
	for _, days := range []int{0, -1, 366} {
		_, err := cp.FleetAnalytics(t.Context(), billing.MerchantID{}, days)
		require.ErrorIs(t, err, billing.ErrInvalid, days)
	}
	for _, weeks := range []int{0, 3, 53} {
		_, err := cp.FleetTimeseries(t.Context(), billing.MerchantID{}, weeks)
		require.ErrorIs(t, err, billing.ErrInvalid, weeks)
	}
	_, err := cp.FleetAnalytics(t.Context(), billing.MerchantID{}, 365)
	require.NotErrorIs(t, err, billing.ErrInvalid)
	_, err = cp.FleetTimeseries(t.Context(), billing.MerchantID{}, 4)
	require.NotErrorIs(t, err, billing.ErrInvalid)
}
