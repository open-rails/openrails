package controlplane

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
	jwtkit "github.com/open-rails/authkit/jwtkit"
	"github.com/open-rails/authkit/verify"
	"github.com/stretchr/testify/require"
)

// Successful stored-authority verification needs AuthKit's live backend and is
// covered end to end; these pin the refusals that need no database.

const (
	testDelegatedIssuer  = "https://openrails.test.example"
	testDelegatedKID     = "test-kid-1"
	testDelegatedSubject = "019aaaab-0000-7000-8000-000000000042"
)

type delegatedRemoteAppSource []authkit.RemoteApplication

func (s delegatedRemoteAppSource) ListEnabledRemoteApplications(context.Context) ([]authkit.RemoteApplication, error) {
	return append([]authkit.RemoteApplication(nil), s...), nil
}

func (s delegatedRemoteAppSource) GetRemoteApplication(_ context.Context, issuer string) (*authkit.RemoteApplication, error) {
	for i := range s {
		if s[i].Issuer == issuer {
			app := s[i]
			return &app, nil
		}
	}
	return nil, authkit.ErrRemoteApplicationNotFound
}

type testHTTPBackend struct{ authcore.HTTPBackend }

func (*testHTTPBackend) ClaimDPoPProof(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}

// newStaticIssuerVerifier trusts one static-key remote application and has no
// authority backend.
func newStaticIssuerVerifier(t *testing.T) (*verify.Verifier, *jwtkit.RSASigner) {
	t.Helper()
	signer, err := jwtkit.NewRSASigner(2048, testDelegatedKID)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(signer.PublicKey())
	require.NoError(t, err)
	v := verify.NewVerifier()
	require.NoError(t, v.LoadRemoteApplications(context.Background(), delegatedRemoteAppSource{{
		ID: "remote-app-1", Slug: "openrails-test", Issuer: testDelegatedIssuer, Mode: authkit.RemoteAppModeStatic, Enabled: true,
		PublicKeys: []authkit.RemoteAppKey{{KID: testDelegatedKID, PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}},
	}}, []string{"openrails"}))
	return v, signer
}

func mintDelegated(t *testing.T, signer jwtkit.Signer, mutate func(jwt.MapClaims)) string {
	t.Helper()
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": testDelegatedIssuer, "aud": []string{"openrails"}, "delegated_sub": testDelegatedSubject,
		"iat": now.Unix(), "exp": now.Add(15 * time.Minute).Unix(),
	}
	if mutate != nil {
		mutate(claims)
	}
	tok, err := jwtkit.SignWithType(context.Background(), signer, claims, jwtkit.DelegatedAccessTokenType, true)
	require.NoError(t, err)
	return tok
}

func TestDelegatedVerifierRefusals(t *testing.T) {
	v, signer := newStaticIssuerVerifier(t)
	for name, tok := range map[string]string{
		"no live authority backend":  mintDelegated(t, signer, nil),
		"self-asserted admission":    mintDelegated(t, signer, func(c jwt.MapClaims) { c["permissions"] = []string{PermMerchantAdmissionsCreate} }),
		"self-asserted root":         mintDelegated(t, signer, func(c jwt.MapClaims) { c["permissions"] = []string{"root:*"} }),
		"wrong audience":             mintDelegated(t, signer, func(c jwt.MapClaims) { c["aud"] = []string{"host-four"} }),
		"expired":                    mintDelegated(t, signer, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() }),
		"unregistered issuer":        mintDelegated(t, signer, func(c jwt.MapClaims) { c["iss"] = "https://evil.example" }),
		"api key is not a delegated": APIKeyPrefix + "_st_keyid_secret",
	} {
		_, _, err := v.VerifyDelegatedAccess(context.Background(), tok)
		require.Error(t, err, name)
	}
}

// BND4-2: merchant jwks_uri fetches go through AuthKit's SSRF-guarding dialer,
// so a loopback JWKS is never reached.
func TestDelegatedVerifierSSRFGuardBlocksLoopbackJWKS(t *testing.T) {
	_, err := newDelegatedVerifier(nil, "", nil)
	require.ErrorIs(t, err, ErrDelegatedNotConfigured)

	signer, err := jwtkit.NewRSASigner(2048, testDelegatedKID)
	require.NoError(t, err)
	var served atomic.Bool
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Store(true)
		jwtkit.ServeJWKS(w, r, jwtkit.JWKS{Keys: []jwtkit.JWK{jwtkit.PublicToJWK(signer.PublicKey(), signer.KID(), signer.Algorithm())}})
	}))
	defer jwks.Close()

	v, err := newDelegatedVerifier(&testHTTPBackend{}, "", nil)
	require.NoError(t, err)
	require.NoError(t, v.AddIssuer(testDelegatedIssuer, []string{"openrails"}, verify.IssuerOptions{JWKSURI: jwks.URL + "/.well-known/jwks.json"}))
	_, _, err = v.VerifyDelegatedAccess(context.Background(), mintDelegated(t, signer, nil))
	require.Error(t, err)
	require.False(t, served.Load(), "the guard refuses the connection before the JWKS endpoint")
}

func TestResolveDelegatedRefusesMalformedInput(t *testing.T) {
	v, _ := newStaticIssuerVerifier(t)
	cp := &ControlPlane{delegatedVerifier: v}
	_, err := cp.ResolveDelegated(nil)
	require.ErrorIs(t, err, ErrDelegatedInvalid)
	_, err = cp.ResolveRemoteApplication(context.Background(), "  ")
	require.ErrorIs(t, err, ErrDelegatedInvalid)
	attrs := map[string]json.RawMessage{"email": json.RawMessage(`" a@example.com "`), "username": json.RawMessage(`42`), "flag": json.RawMessage(`"true"`)}
	require.Equal(t, "a@example.com", delegatedStringAttribute(attrs, "email"))
	require.Empty(t, delegatedStringAttribute(attrs, "username"), "wrong JSON type is ignored")
	require.False(t, delegatedBoolAttribute(attrs, "flag"), "a string is not a boolean")
}

func waitRefreshIdle(t *testing.T, c *ControlPlane) {
	t.Helper()
	require.Eventually(t, func() bool { return !c.issuerRefresh.busy.Load() }, 5*time.Second, 2*time.Millisecond)
}

type panickingIssuerSource struct{ verify.Enricher }

func (panickingIssuerSource) ListEnabledRemoteApplications(context.Context) ([]authkit.RemoteApplication, error) {
	panic("fixture issuer failure")
}

// or#854: a partially configured plane and a panicking refresh must degrade,
// never kill the process from a background goroutine.
func TestIssuerRegistryRefreshDegrades(t *testing.T) {
	v, _ := newStaticIssuerVerifier(t)
	partial := &ControlPlane{delegatedVerifier: v}
	require.ErrorIs(t, partial.loadRemoteApplications(context.Background()), ErrRemoteApplicationSourceUnavailable)
	require.ErrorIs(t, (&ControlPlane{}).loadRemoteApplications(context.Background()), ErrDelegatedNotConfigured)
	partial.SetIssuerRegistryTTL(time.Nanosecond)
	partial.refreshIssuerRegistryIfStale()
	require.False(t, partial.issuerRefresh.busy.Load(), "no refresh goroutine without an issuer source")
	require.Zero(t, partial.issuerRefresh.lastLoad.Load())

	pv, _ := newStaticIssuerVerifier(t)
	pv.WithService(panickingIssuerSource{})
	cp := &ControlPlane{delegatedVerifier: pv, client: struct{ authkit.Client }{}}
	cp.SetIssuerRegistryTTL(time.Nanosecond)
	for range 2 {
		cp.refreshIssuerRegistryIfStale()
		waitRefreshIdle(t, cp)
		require.Zero(t, cp.issuerRefresh.lastLoad.Load(), "a failed refresh stays stale so the next verification retries")
	}

	// A fresh load suppresses refresh until the TTL elapses.
	cp.SetIssuerRegistryTTL(time.Hour)
	cp.issuerRefresh.lastLoad.Store(time.Now().UnixNano())
	cp.refreshIssuerRegistryIfStale()
	require.False(t, cp.issuerRefresh.busy.Load())
}
