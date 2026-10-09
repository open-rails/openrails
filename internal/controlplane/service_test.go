package controlplane

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/credential"
)

// Every refusal precedes AuthKit's construction, so none needs a database.
func TestNewRefusesIncompleteConfiguration(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), "postgres://127.0.0.1:1/unreachable")
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	issuer := "https://openrails.example.com"
	for want, tc := range map[string]struct {
		cfg  *config.Config
		auth *config.AuthConfig
		pool *pgxpool.Pool
		opts []Option
	}{
		"auth.issuer is required":        {&config.Config{}, nil, pool, nil},
		"auth issuer":                    {&config.Config{}, &config.AuthConfig{Issuer: "http://openrails.example.com"}, pool, nil},
		"request_origin":                 {&config.Config{}, &config.AuthConfig{Issuer: issuer, RequestOrigin: "https://a.example/billing"}, pool, nil},
		"pgx pool is required":           {&config.Config{}, &config.AuthConfig{Issuer: issuer}, nil, nil},
		"merchant creation slug pattern": {&config.Config{}, &config.AuthConfig{Issuer: issuer}, pool, []Option{WithMerchantCreation(MerchantCreationConfig{SlugPattern: "("})}},
		"naming policy":                  {&config.Config{}, &config.AuthConfig{Issuer: issuer, Naming: config.NamingConfig{FormerNames: config.FormerNamesConfig{Mode: "sometimes"}}}, pool, nil},
		"explicit client-IP posture":     {&config.Config{}, &config.AuthConfig{Issuer: issuer, MintDisabled: true}, pool, nil},
		"rate limits need Redis":         {&config.Config{}, &config.AuthConfig{Issuer: issuer, DirectPeerIP: true, MintDisabled: true}, pool, nil},
		"AUTHKIT_ACTIVE_KEY_ID":          {&config.Config{}, &config.AuthConfig{Issuer: issuer, ActiveKeyID: "k", ActivePrivateKeyPEM: "not a pem"}, pool, nil},
	} {
		_, err := New(context.Background(), tc.cfg, tc.auth, tc.pool, tc.opts...)
		require.ErrorContains(t, err, want)
	}
}

// Standalone is private by construction: registration is closed unless the
// host opens it, and the mounted AuthKit surface follows the mode.
func TestRegistrationIsClosedUnlessOpened(t *testing.T) {
	cp := &ControlPlane{}
	require.Equal(t, iam.RegistrationModeClosed, cp.Registration())
	groups := cp.MountedRouteGroups()
	require.Equal(t, IntentionalRouteGroups, groups)
	groups[0] = "mutated"
	require.NotEqual(t, "mutated", string(IntentionalRouteGroups[0]), "callers get a copy")
	for _, mode := range []iam.RegistrationMode{iam.RegistrationModeOpen, iam.RegistrationModeInviteOnly} {
		mounted := (&ControlPlane{registration: mode}).MountedRouteGroups()
		require.NotContains(t, mounted, iam.RouteBrowserOIDC, "%s registration still mounts no browser OIDC", mode)
		require.Contains(t, mounted, iam.RouteRegistration, mode)
	}
	require.NotContains(t, (&ControlPlane{registration: iam.RegistrationModeClosed}).MountedRouteGroups(), iam.RouteRegistration)

	auth := &config.AuthConfig{}
	require.Equal(t, authkit.RegistrationConfig{NativeUserMode: iam.RegistrationModeClosed, Verification: iam.RegistrationVerificationNone}, registration(options{}, auth))
	open := registration(newOptions([]Option{WithRegistration(iam.RegistrationModeOpen), WithPasswordless(true)}), auth)
	require.Equal(t, authkit.RegistrationConfig{NativeUserMode: iam.RegistrationModeOpen, Verification: iam.RegistrationVerificationRequired, PasswordlessLogin: true, PasswordlessAutoRegistration: true}, open)
	invite := registration(newOptions([]Option{WithRegistration(iam.RegistrationModeInviteOnly)}), auth)
	require.Equal(t, authkit.RegistrationConfig{NativeUserMode: iam.RegistrationModeInviteOnly, Verification: iam.RegistrationVerificationRequired}, invite)
	require.ErrorContains(t, ValidateRegistrationMode("sometimes"), "open, invite_only, closed")

	defaults := newOptions([]Option{nil})
	require.False(t, defaults.registration != "" || defaults.passwordlessLogin || defaults.passwordlessAutoRegistration || defaults.merchantCreation != nil)
	login := newOptions([]Option{WithPasswordless(false)})
	require.True(t, login.passwordlessLogin && !login.passwordlessAutoRegistration)
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

// The site naming policy governs usernames too: OpenRails' zero interval is
// "no wait", AuthKit's is its default.
func TestUsernamePolicyFollowsNaming(t *testing.T) {
	week, zero := 7*24*time.Hour, time.Duration(0)
	p, err := config.NormalizeNaming(config.NamingConfig{RenameInterval: &zero, FormerNames: config.FormerNamesConfig{Duration: &week}})
	require.NoError(t, err)
	require.Equal(t, authkit.UsernameConfig{Renames: true, RenameInterval: -1, FormerNames: authkit.FormerNamesConfig{Mode: authkit.FormerNamesFinite, Duration: week}}, usernames(p))
	p, err = config.NormalizeNaming(config.NamingConfig{FormerNames: config.FormerNamesConfig{Mode: config.FormerNamesForever}})
	require.NoError(t, err)
	require.Equal(t, authkit.UsernameConfig{Renames: true, RenameInterval: 72 * time.Hour, FormerNames: authkit.FormerNamesConfig{Mode: authkit.FormerNamesForever}}, usernames(p))
}

// ak#299: an undeclared client-IP posture refuses boot rather than sharing one
// rate-limit bucket behind an unknown proxy.
func TestClientIPPostureMustBeDeclared(t *testing.T) {
	proxies := []string{"10.0.0.0/8"}
	for _, tc := range []struct {
		cfg     config.Config
		direct  bool
		opts    options
		wantErr string
	}{
		{wantErr: "explicit client-IP posture"},
		{direct: true},
		{cfg: config.Config{TrustedProxies: proxies}},
		{cfg: config.Config{TrustedProxies: []string{"192.168.0.0/16"}}, opts: options{trustedProxies: proxies}},
		{cfg: config.Config{CloudflareProxies: proxies}},
		{cfg: config.Config{TrustedProxies: proxies}, direct: true, wantErr: "conflicts"},
		{cfg: config.Config{CloudflareProxies: proxies}, opts: options{directPeerIP: true}, wantErr: "conflicts"},
	} {
		cfg := tc.cfg
		got, err := clientIPPosture(&cfg, &config.AuthConfig{DirectPeerIP: tc.direct}, tc.opts)
		if tc.wantErr != "" {
			require.ErrorContains(t, err, tc.wantErr)
			continue
		}
		require.NoError(t, err)
		require.Equal(t, tc.direct || tc.opts.directPeerIP, got.DirectPeerIP)
		if len(cfg.TrustedProxies) > 0 {
			require.Equal(t, proxies, got.TrustedProxies, "options override config")
		}
	}
}

// #752: inline keys cannot rotate, so they warn; without them keys_path
// resolves in AuthKit.
func TestInlineKeySource(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	hook := logtest.NewGlobal()
	defer hook.Reset()
	warned := func() bool {
		for _, e := range hook.AllEntries() {
			if e.Level == log.WarnLevel && strings.Contains(e.Message, "inline PEM") && strings.Contains(e.Message, "FROZEN") {
				return true
			}
		}
		return false
	}

	ks, err := inlineKeySource(&config.AuthConfig{ActiveKeyID: "k", ActivePrivateKeyPEM: keyPEM})
	require.NoError(t, err)
	require.Equal(t, "k", ks.ActiveSigner().KID())
	require.True(t, warned())
	_, err = inlineKeySource(&config.AuthConfig{ActiveKeyID: "k", ActivePrivateKeyPEM: keyPEM, PublicKeysJSON: "{"})
	require.ErrorContains(t, err, "AUTHKIT_PUBLIC_KEYS")
	_, err = inlineKeySource(&config.AuthConfig{ActiveKeyID: "k", ActivePrivateKeyPEM: "not a pem"})
	require.Error(t, err)

	hook.Reset()
	ks, err = inlineKeySource(&config.AuthConfig{KeysPath: t.TempDir()})
	require.NoError(t, err)
	require.Nil(t, ks, "keys_path is resolved by AuthKit")
	require.False(t, warned(), "keys_path is the hot-rotating path")
}

// A missing or partial control plane fails closed with a typed error, never a panic.
func TestUnconfiguredControlPlaneFailsClosed(t *testing.T) {
	ctx := context.Background()
	mid := billing.MerchantID{1}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, cp := range []*ControlPlane{nil, {}} {
		require.Equal(t, APIKeyPrefix, cp.TokenPrefix())
		require.True(t, cp.LooksLikeAPIKey(" "+APIKeyPrefix+"_st_key_secret "))
		require.False(t, cp.LooksLikeAPIKey("eyJ.a.b"))
		require.Nil(t, cp.UserAuthenticator())
		require.Nil(t, cp.AuthHandler())
		require.Empty(t, cp.AuthRoutes())
		require.Empty(t, cp.AuthAPIBase())
		_, err := cp.ResolveAPIKey(ctx, APIKeyPrefix+"_st_key_secret")
		require.ErrorIs(t, err, ErrNoControlPlane)
		_, err = cp.ResolveResourceToken(r)
		require.ErrorIs(t, err, credential.ErrResourceServerNotConfigured)
		_, err = cp.ResolveResourceUser(r)
		require.ErrorIs(t, err, credential.ErrResourceServerNotConfigured)
		_, err = cp.ResolveResourceCustomer(r)
		require.ErrorIs(t, err, credential.ErrResourceServerNotConfigured)
		_, _, err = cp.MintMerchantAPIKey(ctx, mid, "k", MerchantViewer, iam.SystemActor())
		require.ErrorIs(t, err, ErrNoControlPlane)
		_, _, err = cp.ResolveAuthorizedMerchant(ctx, r, "acme", "merchant:catalog:read")
		require.ErrorIs(t, err, ErrNoControlPlane)
		_, err = cp.HasRootPermission(ctx, r, "root:merchants:read")
		require.ErrorIs(t, err, ErrNoControlPlane)
		_, err = cp.RequestActor(r)
		require.ErrorIs(t, err, ErrNoControlPlane)
		_, _, err = cp.MerchantScope(ctx, "acme")
		require.ErrorIs(t, err, ErrServiceCredentialMerchantUnresolved)
		require.Error(t, cp.AuthorizeMerchant(ctx, "group", mid))
		cp.Close()
	}
	for _, bad := range []struct {
		mid     billing.MerchantID
		subject string
	}{{billing.MerchantID{}, "11111111-1111-4111-8111-111111111111"}, {mid, ""}, {mid, "not-a-uuid"}} {
		_, err := (&ControlPlane{}).TouchCustomer(ctx, bad.mid, "iss", bad.subject)
		require.ErrorIs(t, err, ErrCustomerInvalid)
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
