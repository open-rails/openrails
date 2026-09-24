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

	authcore "github.com/open-rails/authkit/embedded"
	jwtkit "github.com/open-rails/authkit/jwtkit"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestNewRefusesIncompleteConfiguration(t *testing.T) {
	for want, tc := range map[string]struct {
		cfg  *config.Config
		auth *hostconfig.AuthConfig
	}{
		"auth.issuer is required": {&config.Config{}, nil},
		"auth issuer":             {&config.Config{}, &hostconfig.AuthConfig{Issuer: "http://openrails.example.com"}},
		"request_origin":          {&config.Config{}, &hostconfig.AuthConfig{Issuer: "https://a.example", RequestOrigin: "https://a.example/billing"}},
		"pgx pool is required":    {&config.Config{}, &hostconfig.AuthConfig{Issuer: "https://openrails.example.com"}},
	} {
		_, err := New(context.Background(), tc.cfg, tc.auth, nil)
		require.ErrorContains(t, err, want)
	}
}

// Standalone is private by construction; hosted/passwordless are code-only opt-ins.
func TestPostureIsCodeOnlyOptIn(t *testing.T) {
	cp := &ControlPlane{}
	require.True(t, cp.SelfHostedPosture())
	groups := cp.MountedRouteGroups()
	require.Equal(t, IntentionalRouteGroups, groups)
	groups[0] = "mutated"
	require.NotEqual(t, "mutated", string(IntentionalRouteGroups[0]), "callers get a copy")
	require.Equal(t, authcore.RegistrationModeClosed, registrationMode(true))
	require.Equal(t, authcore.RegistrationVerificationNone, registrationVerification(true))
	require.Nil(t, (&ControlPlane{hosted: true}).MountedRouteGroups(), "hosted without an auth service mounts nothing")
	require.Equal(t, authcore.RegistrationModeOpen, registrationMode(false))
	require.Equal(t, authcore.RegistrationVerificationRequired, registrationVerification(false))

	defaults := newOptions([]Option{nil})
	require.False(t, defaults.hosted || defaults.passwordlessLogin || defaults.passwordlessAutoRegistration || defaults.merchantCreation != nil)
	require.True(t, newOptions([]Option{WithHostedPosture()}).hosted)
	login := newOptions([]Option{WithPasswordless(false)})
	require.True(t, login.passwordlessLogin && !login.passwordlessAutoRegistration)
	auto := newOptions([]Option{WithPasswordless(true)})
	require.True(t, auto.passwordlessLogin && auto.passwordlessAutoRegistration)

	require.Equal(t, authcore.FrontendConfig{BaseURL: "https://issuer.example"}, resolveFrontendConfig("https://issuer.example", authcore.FrontendConfig{}))
	override := authcore.FrontendConfig{BaseURL: "https://app.example"}
	require.Equal(t, override, resolveFrontendConfig("https://issuer.example", override))
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
		got, err := clientIPPosture(&cfg, &hostconfig.AuthConfig{DirectPeerIP: tc.direct}, tc.opts)
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

// #752: inline keys cannot rotate, so they warn; a missing key fails closed
// unless ephemeral keys are explicitly allowed.
func TestControlPlaneKeySource(t *testing.T) {
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
	cfg := &config.Config{}

	_, err = resolveControlPlaneKeySource(cfg, &hostconfig.AuthConfig{ActiveKeyID: "k", ActivePrivateKeyPEM: keyPEM})
	require.NoError(t, err)
	require.True(t, warned())
	_, err = resolveControlPlaneKeySource(cfg, &hostconfig.AuthConfig{ActiveKeyID: "k", ActivePrivateKeyPEM: keyPEM, PublicKeysJSON: "{"})
	require.ErrorContains(t, err, "AUTHKIT_PUBLIC_KEYS")
	_, err = resolveControlPlaneKeySource(cfg, &hostconfig.AuthConfig{ActiveKeyID: "k", ActivePrivateKeyPEM: "not a pem"})
	require.Error(t, err)

	hook.Reset()
	auth := &hostconfig.AuthConfig{KeysPath: t.TempDir()}
	_, err = resolveControlPlaneKeySource(cfg, auth)
	require.Error(t, err, "missing key fails closed")
	auth.AllowEphemeralSigningKey = true
	_, err = resolveControlPlaneKeySource(cfg, auth)
	require.NoError(t, err)
	require.False(t, warned(), "keys_path is the hot-rotating path")
	require.Equal(t, jwtkit.DefaultAuthKeysPath, authKeysPath(&hostconfig.AuthConfig{KeysPath: "  "}))
}

// DPoP htu comes from trusted configuration, never Host/X-Forwarded-Host.
func TestDelegatedRequestURLUsesTrustedOrigin(t *testing.T) {
	for _, path := range []string{"/billing/v1/orders", "/billing/v1/a%2Fb", "/billing/v1/a//b"} {
		r := httptest.NewRequest(http.MethodGet, "http://untrusted.test"+path+"?q=x", nil)
		r.Header.Set("X-Forwarded-Host", "attacker.test")
		require.Equal(t, "https://billing.example"+path, delegatedRequestURL("https://billing.example/", nil)(r))
	}
	r := httptest.NewRequest(http.MethodGet, "http://untrusted.test/billing/v1/a", nil)
	for _, bad := range []string{"", "billing.example", "ftp://billing.example", "https://billing.example/billing", "https://user:secret@billing.example", "https://billing.example?override=yes", "https://billing.example#x"} {
		require.Empty(t, delegatedRequestURL(bad, nil)(r), bad)
	}
	override := func(*http.Request) string { return "https://host.example/original%2Fpath" }
	require.Equal(t, "https://host.example/original%2Fpath", delegatedRequestURL("", override)(r))
}

// A missing or partial control plane fails closed with a typed error, never a panic.
func TestUnconfiguredControlPlaneFailsClosed(t *testing.T) {
	ctx := context.Background()
	mid := merchant.ID{1}
	for _, cp := range []*ControlPlane{nil, {}} {
		require.Equal(t, APIKeyPrefix, cp.TokenPrefix())
		require.True(t, cp.LooksLikeAPIKey(" "+APIKeyPrefix+"_st_key_secret "))
		require.False(t, cp.LooksLikeAPIKey("eyJ.a.b"))
		require.Nil(t, cp.UserAuthenticator())
		_, err := cp.ResolveAPIKey(ctx, APIKeyPrefix+"_st_key_secret")
		require.ErrorIs(t, err, ErrNoControlPlane)
		_, err = cp.ResolveServiceJWT(ctx, "a.b.c")
		require.ErrorIs(t, err, ErrNoControlPlane)
		_, err = cp.ResolveDelegated(httptest.NewRequest(http.MethodGet, "/", nil))
		require.ErrorIs(t, err, ErrDelegatedNotConfigured)
		_, err = cp.ResolveRemoteApplication(ctx, "a.b.c")
		require.ErrorIs(t, err, ErrRemoteApplicationNotConfigured)
		_, _, err = cp.MintMerchantAPIKey(ctx, mid, "k", MerchantRoleViewer, "")
		require.ErrorIs(t, err, ErrNoControlPlane)
		_, _, err = cp.ResolveAuthorizedMerchant(ctx, "acme", "user", PermMerchantCatalogRead)
		require.ErrorIs(t, err, ErrNoControlPlane)
		_, _, err = cp.MerchantScope(ctx, "acme")
		require.ErrorIs(t, err, ErrServiceCredentialMerchantUnresolved)
		require.Error(t, cp.AuthorizeMerchant(ctx, "group", mid))
		_, _, _, _, _, err = cp.merchantForIssuer(ctx, "  ")
		require.ErrorIs(t, err, ErrDelegatedIssuerUnknown)
		cp.Close()
	}
	for _, bad := range []struct {
		mid     merchant.ID
		subject string
	}{{merchant.ID{}, "11111111-1111-4111-8111-111111111111"}, {mid, ""}, {mid, "not-a-uuid"}} {
		_, err := (&ControlPlane{}).TouchCustomer(ctx, bad.mid, "iss", bad.subject)
		require.ErrorIs(t, err, ErrCustomerInvalid)
	}
}
