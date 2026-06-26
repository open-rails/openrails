package controlplane

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
	authhttp "github.com/open-rails/authkit/http"
	jwtkit "github.com/open-rails/authkit/jwt"
	"github.com/stretchr/testify/require"
)

// These tests exercise the SECURITY-CRITICAL delegated-access-token verification
// decisions the browser-direct self-service surface depends on (issue #222
// browser tier): canonical audience enforcement, AuthKit delegated profile +
// no-`sub`
// requirement, and the self permission gate. They build the
// exact verifier configuration newDelegatedVerifier uses, so they pin the real
// behavior without needing a database (the AuthKit org -> OpenRails merchant mapping in
// ResolveDelegated keys on the registered issuer, covered by the API key path + the
// middleware tests).

const (
	testDelegatedIssuer  = "https://openrails.test.example"
	testDelegatedKID     = "test-kid-1"
	canonicalAudience    = "openrails"
	testDelegatedSubject = "end-user-42"
	// testDelegatedOrg / testDelegatedOrgID are LEGACY claim values used
	// only to prove the issuer-only hard cut (#361, authkit v0.23.0): tokens
	// carrying `tenant` (slug) or `tenant_id` (uuid) are rejected.
	testDelegatedOrg   = "operator"
	testDelegatedOrgID = "11111111-1111-1111-1111-111111111111"
	wrongAudience      = "tensorhub"
)

// newTestDelegatedVerifier builds a Verifier identical to newDelegatedVerifier's
// configuration (issuer, openrails audience, local public key, NO permission
// allowlist — #564), seeded with a freshly generated signing key.
func newTestDelegatedVerifier(t *testing.T) (*authhttp.Verifier, jwtkit.Signer) {
	t.Helper()
	signer, err := jwtkit.NewRSASigner(2048, testDelegatedKID)
	require.NoError(t, err)

	v := authhttp.NewVerifier()
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

func mintDelegated(t *testing.T, signer jwtkit.Signer, p authhttp.DelegatedAccessParams) string {
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
	tok, err := authhttp.MintDelegatedAccessToken(context.Background(), signer, p)
	require.NoError(t, err)
	return tok
}

type delegatedRemoteAppSource []authkit.RemoteApplication

func (s delegatedRemoteAppSource) ListRemoteApplications(context.Context, bool) ([]authkit.RemoteApplication, error) {
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

func TestDelegatedVerify_SucceedsWithoutPermissionsAndAudience(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{})
	cl, dp, err := v.VerifyDelegatedAccess(tok)
	require.NoError(t, err)
	require.Equal(t, "", cl.UserID, "delegated token must not carry a normal sub")
	require.Equal(t, testDelegatedSubject, dp.DelegatedSubject)
	require.Equal(t, testDelegatedIssuer, dp.Issuer, "the validated iss is the merchant issuer identity")
	require.Empty(t, dp.Permissions)
}

func TestResolveDelegatedRejectsOriginNotAllowedByIssuer(t *testing.T) {
	signer, err := jwtkit.NewRSASigner(2048, testDelegatedKID)
	require.NoError(t, err)
	jwks := testJWKS(t, signer)
	defer jwks.Close()

	v, err := newDelegatedVerifier(&authcore.Client{}, "")
	require.NoError(t, err)
	require.NoError(t, v.LoadRemoteApplications(context.Background(), delegatedRemoteAppSource{{
		Slug:           "doujins",
		Issuer:         testDelegatedIssuer,
		JWKSURI:        jwks.URL + "/.well-known/jwks.json",
		AllowedOrigins: []string{"https://doujins.com"},
		Enabled:        true,
	}}, []string{canonicalAudience}))
	cp := &ControlPlane{delegatedVerifier: v}
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{})

	_, err = cp.ResolveDelegated(context.Background(), tok, "https://evil.example")
	require.ErrorIs(t, err, ErrDelegatedOriginNotAllowed)
}

func TestDelegatedVerify_RejectsWrongAudience(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{
		Audiences: []string{wrongAudience},
	})
	_, _, err := v.VerifyDelegatedAccess(tok)
	require.Error(t, err, "a token whose aud does not include openrails must be rejected")
}

// #564: a delegated token can carry ANY permission the SIGNING remote-app holds —
// including merchant:admissions:create, which the deleted #259 browser-safe allowlist
// used to block. AuthKit bounds the claim to the signer's stored authority (the test
// remote-app holds the full catalog), so an in-authority claim is carried.
func TestDelegatedVerify_CarriesInAuthorityPermIncludingAdmit(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{
		Permissions: []string{PermMerchantAdmissionsCreate},
	})
	_, dp, err := v.VerifyDelegatedAccess(tok)
	require.NoError(t, err, "admit is carriable on a delegated token when the signer holds it (#564)")
	require.Contains(t, dp.Permissions, PermMerchantAdmissionsCreate)
}

// #567 (authkit v0.50.0 permission-group hard cut): the delegated `permissions`
// bound against the signer's STORED authority no longer happens at this
// verifier seam — the org-membership-backed authority resolver was removed and
// its permission-group replacement is wired at the core/enricher seam (see
// authkit verify/verifier.go resolveRemoteApplicationSelf). A bare verifier
// (no WithService enricher, as newTestDelegatedVerifier builds) therefore carries
// the claim through verbatim; OpenRails' route gate is what bounds it via the
// glob-aware HasPermission check on every credential type (#565). This test pins
// the new contract: a foreign-persona claim verifies (it is not bounded here) and
// is surfaced for the route gate to reject.
func TestDelegatedVerify_CarriesClaimForRouteGateBound(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{
		Permissions: []string{"root:*"},
	})
	_, dp, err := v.VerifyDelegatedAccess(tok)
	require.NoError(t, err, "without a WithService enricher the verifier does not bound the claim (#567)")
	require.Contains(t, dp.Permissions, "root:*")
	// The OpenRails gate denies it: a foreign-persona glob covers no merchant perm.
	require.False(t, (&ResolvedDelegated{Permissions: dp.Permissions}).HasPermission(PermMerchantAdmissionsCreate))
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
	_, _, verr := v.VerifyDelegatedAccess(tok)
	require.Error(t, verr, "expired delegated token must be rejected")
}

// #567 (authkit v0.50.0 permission-group hard cut): there is NO `org` persona, so
// a delegated token's legacy `org_id`/`org` claims are INERT — they name nothing
// the verifier interprets and never leak into the principal. (Pre-v0.50 authkit
// rejected them under the issuer-only profile; the rejection is gone with the org
// concept itself. Merchant identity is the validated `iss` -> merchant group.)
func TestDelegatedVerify_OrgIDClaimIsInert(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	hs := signer.(jwtkit.HeaderSigner)
	now := time.Now()
	tok, err := hs.SignWithHeaders(context.Background(),
		jwt.MapClaims{
			"iss":           testDelegatedIssuer,
			"aud":           []string{canonicalAudience},
			"org_id":        testDelegatedOrgID,
			"delegated_sub": testDelegatedSubject,
			"iat":           now.Unix(),
			"exp":           now.Add(time.Minute).Unix(),
		},
		map[string]any{"typ": authhttp.DelegatedAccessTokenType},
	)
	require.NoError(t, err)
	cl, dp, verr := v.VerifyDelegatedAccess(tok)
	require.NoError(t, verr, "an org_id claim is inert under the permission-group model (#567)")
	require.Equal(t, testDelegatedSubject, dp.DelegatedSubject)
	require.Empty(t, cl.UserID, "still no normal sub")
}

// Twin of the org_id case: an `org` slug claim is equally inert. Tokens carry no
// org concept; identity is the validated issuer.
func TestDelegatedVerify_OrgClaimIsInert(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	hs := signer.(jwtkit.HeaderSigner)
	now := time.Now()
	tok, err := hs.SignWithHeaders(context.Background(),
		jwt.MapClaims{
			"iss":           testDelegatedIssuer,
			"aud":           []string{canonicalAudience},
			"org":           testDelegatedOrg,
			"delegated_sub": testDelegatedSubject,
			"iat":           now.Unix(),
			"exp":           now.Add(time.Minute).Unix(),
		},
		map[string]any{"typ": authhttp.DelegatedAccessTokenType},
	)
	require.NoError(t, err)
	_, dp, verr := v.VerifyDelegatedAccess(tok)
	require.NoError(t, verr, "an org slug claim is inert under the permission-group model (#567)")
	require.Equal(t, testDelegatedSubject, dp.DelegatedSubject)
}

func TestDelegatedVerify_RejectsServiceCredential(t *testing.T) {
	v, _ := newTestDelegatedVerifier(t)
	_, _, err := v.VerifyDelegatedAccess("openrails_st_keyid_secret")
	require.Error(t, err, "API keys must not verify as delegated browser tokens")
}

// guard: ensure authcore error sentinels are wired for the resolver's mapping.
var _ = authkit.ErrAccessTokenExpired
