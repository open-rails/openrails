package controlplane

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/open-rails/authkit"
	authhttp "github.com/open-rails/authkit/authhttp"
	authcore "github.com/open-rails/authkit/embedded"
	jwtkit "github.com/open-rails/authkit/jwtkit"
	"github.com/open-rails/authkit/verify"
	"github.com/stretchr/testify/require"
)

// Parser/profile refusals stay cheap. Successful stored-authority verification
// is covered by the real PostgreSQL workflow in delegated_integration_test.go.

const (
	testDelegatedIssuer  = "https://openrails.test.example"
	testDelegatedKID     = "test-kid-1"
	canonicalAudience    = "openrails"
	testDelegatedSubject = "019aaaab-0000-7000-8000-000000000042"
	wrongAudience        = "host-four"
)

// newTestDelegatedVerifier deliberately has no authority backend, for refusal
// and partially configured registry tests. Production installs the AuthKit core.
func newTestDelegatedVerifier(t *testing.T) (*verify.Verifier, jwtkit.Signer) {
	t.Helper()
	signer, err := jwtkit.NewRSASigner(2048, testDelegatedKID)
	require.NoError(t, err)

	v := verify.NewVerifier()
	require.NoError(t, v.LoadRemoteApplications(context.Background(), delegatedRemoteAppSource{{
		ID:      "remote-app-1",
		Slug:    "openrails-test",
		Issuer:  testDelegatedIssuer,
		Mode:    authkit.RemoteAppModeStatic,
		Enabled: true,
		PublicKeys: []authkit.RemoteAppKey{{
			KID:          testDelegatedKID,
			PublicKeyPEM: testPublicKeyPEM(t, signer.PublicKey()),
		}},
	}}, []string{canonicalAudience}))
	return v, signer
}

func testPublicKeyPEM(t *testing.T, pub crypto.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func mintDelegated(t *testing.T, signer jwtkit.Signer, p authkit.DelegatedAccessParams) string {
	t.Helper()
	if p.Issuer == "" {
		p.Issuer = testDelegatedIssuer
	}
	if len(p.Audiences) == 0 {
		p.Audiences = []string{canonicalAudience}
	}
	if p.DelegatedSubject == "" {
		p.DelegatedSubject = testDelegatedSubject
	}
	ttl := p.TTL
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	now := time.Now()
	// Mirror authkit's delegated-token claim shape and sign with the delegated
	// JOSE typ. The package-level minter became a Client method in the authkit
	// restructure; this reproduces the same token from a raw signer for the
	// verifier tests below (which need no engine/DB).
	claims := jwt.MapClaims{
		"iss":           p.Issuer,
		"iat":           now.Unix(),
		"exp":           now.Add(ttl).Unix(),
		"delegated_sub": p.DelegatedSubject,
	}
	if len(p.Audiences) > 0 {
		claims["aud"] = p.Audiences
	}
	if len(p.Permissions) > 0 {
		claims["permissions"] = p.Permissions
	}
	tok, err := jwtkit.SignWithType(context.Background(), signer, claims, jwtkit.DelegatedAccessTokenType, true)
	require.NoError(t, err)
	return tok
}

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

func testJWKS(t *testing.T, signer *jwtkit.RSASigner) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		jwk := jwtkit.PublicToJWK(signer.PublicKey(), signer.KID(), signer.Algorithm())
		jwtkit.ServeJWKS(w, r, jwtkit.JWKS{Keys: []jwtkit.JWK{jwk}})
	})
	return httptest.NewServer(mux)
}

func TestDelegatedVerifyRequiresLiveAuthority(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	for _, permissions := range [][]string{nil, {PermMerchantAdmissionsCreate}, {"root:*"}} {
		tok := mintDelegated(t, signer, authkit.DelegatedAccessParams{Permissions: permissions})
		_, _, err := v.VerifyDelegatedAccess(context.Background(), tok)
		require.Error(t, err, "a stored issuer requires the live authority backend")
	}
}

// TestDelegatedVerifier_SSRFGuardBlocksLoopbackJWKS pins BND4-2: newDelegatedVerifier
// MUST install AuthKit's SSRF-guarding dialer, so a JWKS-mode merchant issuer whose
// jwks_uri resolves to a private/loopback address cannot drive an outbound fetch from
// the control-plane host (cloud metadata / internal services). validateJWKSURI is only
// a syntactic registration check (no DNS resolution), so this fetch-time guard is the
// real defense against DNS-rebinding. httptest servers bind to 127.0.0.1, so the guard
// must refuse the connection and the JWKS endpoint must never be reached. If someone
// drops WithSSRFGuard(), the fetch succeeds, `served` flips true, and this test fails.
func TestDelegatedVerifier_SSRFGuardBlocksLoopbackJWKS(t *testing.T) {
	signer, err := jwtkit.NewRSASigner(2048, testDelegatedKID)
	require.NoError(t, err)

	var served atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		served.Store(true)
		jwk := jwtkit.PublicToJWK(signer.PublicKey(), signer.KID(), signer.Algorithm())
		jwtkit.ServeJWKS(w, r, jwtkit.JWKS{Keys: []jwtkit.JWK{jwk}})
	})
	jwks := httptest.NewServer(mux) // binds to 127.0.0.1 — a private/reserved address
	defer jwks.Close()

	v, err := newDelegatedVerifier(&authcore.Runtime{}, "", nil)
	require.NoError(t, err)
	require.NoError(t, v.AddIssuer(testDelegatedIssuer, []string{canonicalAudience}, verify.IssuerOptions{
		JWKSURI: jwks.URL + "/.well-known/jwks.json",
	}))

	tok := mintDelegated(t, signer, authkit.DelegatedAccessParams{})
	_, _, err = v.VerifyDelegatedAccess(context.Background(), tok)
	require.Error(t, err, "SSRF guard must block a loopback jwks_uri fetch, failing verification")
	require.False(t, served.Load(), "the SSRF guard must refuse the connection before the JWKS endpoint is reached")
}

func TestDelegatedVerify_RejectsWrongAudience(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authkit.DelegatedAccessParams{
		Audiences: []string{wrongAudience},
	})
	_, _, err := v.VerifyDelegatedAccess(context.Background(), tok)
	require.Error(t, err, "a token whose aud does not include openrails must be rejected")
}

func TestDelegatedVerify_RejectsExpired(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	// Sign a delegated token shape directly with an `exp` well in the past
	// (MintDelegatedAccessToken clamps non-positive TTL up to 15m, so we cannot
	// express "already expired" through it — sign the canonical claims by hand).
	hs := signer.(jwtkit.HeaderSigner)
	past := time.Now().Add(-2 * time.Hour)
	tok, err := hs.SignWithHeaders(context.Background(),
		jwt.MapClaims{
			"iss":           testDelegatedIssuer,
			"aud":           []string{canonicalAudience},
			"delegated_sub": testDelegatedSubject,
			"iat":           past.Unix(),
			"exp":           past.Add(time.Minute).Unix(),
		},
		map[string]any{"typ": authhttp.DelegatedAccessTokenType},
	)
	require.NoError(t, err)
	_, _, verr := v.VerifyDelegatedAccess(context.Background(), tok)
	require.Error(t, verr, "expired delegated token must be rejected")
}

func TestDelegatedVerify_RejectsServiceCredential(t *testing.T) {
	v, _ := newTestDelegatedVerifier(t)
	_, _, err := v.VerifyDelegatedAccess(context.Background(), "openrails_st_keyid_secret")
	require.Error(t, err, "API keys must not verify as delegated browser tokens")
}

// guard: ensure authcore error sentinels are wired for the resolver's mapping.
var _ = authkit.ErrAccessTokenExpired
