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
	authcore "github.com/open-rails/authkit/core"
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
		Mode:    authcore.RemoteAppModeStatic,
		Enabled: true,
		PublicKeys: []authcore.RemoteAppKey{{
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

type delegatedRemoteAppSource []authcore.RemoteApplication

func (s delegatedRemoteAppSource) ListRemoteApplications(context.Context, bool) ([]authcore.RemoteApplication, error) {
	return append([]authcore.RemoteApplication(nil), s...), nil
}

func (s delegatedRemoteAppSource) GetRemoteApplication(_ context.Context, issuer string) (*authcore.RemoteApplication, error) {
	for i := range s {
		if s[i].Issuer == issuer {
			app := s[i]
			return &app, nil
		}
	}
	return nil, authcore.ErrRemoteApplicationNotFound
}

func (s delegatedRemoteAppSource) ResolveRemoteApplicationAuthority(_ context.Context, appID string) ([]authcore.OrgMembership, []string, error) {
	for i := range s {
		if s[i].ID == appID {
			// #564: the test remote-app's stored authority is the full catalog;
			// ResolveDelegated intersects the token claim against this.
			return nil, CatalogNames(), nil
		}
	}
	return nil, nil, authcore.ErrRemoteApplicationNotFound
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

	v, err := newDelegatedVerifier(&authcore.Service{}, "")
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

// #564 (reject, not drop): a claim BEYOND the signer's stored authority rejects the
// whole token — AuthKit fails loud rather than silently dropping the excess perm.
func TestDelegatedVerify_RejectsOverClaimBeyondSignerAuthority(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	// org:* is not in the remote-app's stored authority (the OpenRails catalog).
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{
		Permissions: []string{authcore.OrgOwnerGrant},
	})
	_, _, err := v.VerifyDelegatedAccess(tok)
	require.Error(t, err, "a perm beyond the signer's authority must reject the whole token")
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

// HARD CUT (#361, authkit v0.23.0): the issuer-only delegated-token profile.
// Org identity is the validated `iss` ONLY; a token still carrying an `org_id`
// uuid claim is rejected outright — there is no legacy-acceptance window.
// (MintDelegatedAccessToken can no longer write the claim, so this signs the
// forbidden shape by hand.)
func TestDelegatedVerify_RejectsOrgIDClaim(t *testing.T) {
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
	_, _, verr := v.VerifyDelegatedAccess(tok)
	require.Error(t, verr, "a delegated token carrying an org_id claim must be rejected (issuer-only hard cut)")
}

// Twin of the org_id rejection: the `org` slug claim is equally forbidden.
// Tokens carry NO org claims of any kind.
func TestDelegatedVerify_RejectsOrgClaim(t *testing.T) {
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
	_, _, verr := v.VerifyDelegatedAccess(tok)
	require.Error(t, verr, "a delegated token carrying an org slug claim must be rejected (issuer-only hard cut)")
}

func TestDelegatedVerify_RejectsServiceCredential(t *testing.T) {
	v, _ := newTestDelegatedVerifier(t)
	_, _, err := v.VerifyDelegatedAccess("openrails_st_keyid_secret")
	require.Error(t, err, "API keys must not verify as delegated browser tokens")
}

// guard: ensure authcore error sentinels are wired for the resolver's mapping.
var _ = authcore.ErrAccessTokenExpired
