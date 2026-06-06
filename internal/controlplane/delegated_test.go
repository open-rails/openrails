package controlplane

import (
	"context"
	"crypto"
	"errors"
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
// requirement, and the openrails:self:* permission-catalog gate. They build the
// exact verifier configuration newDelegatedVerifier uses, so they pin the real
// behavior without needing a database (the org->tenant mapping in
// ResolveDelegated reuses tenantForAuthKitTenantSlug, covered by the service token path + the
// middleware tests).

const (
	testDelegatedIssuer  = "https://billing.test.example"
	testDelegatedKID     = "test-kid-1"
	canonicalAudience    = "openrails"
	testDelegatedSubject = "end-user-42"
	testDelegatedTenant  = "operator"
	wrongAudience        = "tensorhub"
)

// newTestDelegatedVerifier builds a Verifier identical to newDelegatedVerifier's
// configuration (issuer, openrails audience, local public key, self-permission
// catalog validator), seeded with a freshly generated signing key.
func newTestDelegatedVerifier(t *testing.T) (*authhttp.Verifier, jwtkit.Signer) {
	t.Helper()
	signer, err := jwtkit.NewRSASigner(2048, testDelegatedKID)
	require.NoError(t, err)

	v := authhttp.NewVerifier(
		authhttp.WithTenantMode("multi"),
		authhttp.WithPermissionCatalog(func(permissions []string) error {
			if len(permissions) == 0 {
				return errors.New("delegated token carries no permissions")
			}
			for _, p := range permissions {
				if !IsDelegatedPermission(p) {
					return errors.New("permission not in delegated catalog: " + p)
				}
			}
			return nil
		}),
	)
	pub := signer.PublicKey()
	require.NoError(t, v.AddIssuer(testDelegatedIssuer, []string{canonicalAudience}, authhttp.IssuerOptions{
		RawKeys: map[string]crypto.PublicKey{testDelegatedKID: pub},
	}))
	return v, signer
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
	if p.Tenant == "" {
		p.Tenant = testDelegatedTenant
	}
	tok, err := authhttp.MintDelegatedAccessToken(context.Background(), signer, p)
	require.NoError(t, err)
	return tok
}

func TestDelegatedVerify_SucceedsWithSelfPermissionAndAudience(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{
		Permissions: []string{PermSelfBillingRead, PermSelfCheckoutCreate},
	})
	cl, dp, err := v.VerifyDelegatedAccess(tok)
	require.NoError(t, err)
	require.Equal(t, "", cl.UserID, "delegated token must not carry a normal sub")
	require.Equal(t, testDelegatedSubject, dp.DelegatedSubject)
	require.Equal(t, testDelegatedTenant, dp.Tenant)
	require.Contains(t, dp.Permissions, PermSelfBillingRead)
}

func TestDelegatedVerify_RejectsWrongAudience(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{
		Audiences:   []string{wrongAudience},
		Permissions: []string{PermSelfBillingRead},
	})
	_, _, err := v.VerifyDelegatedAccess(tok)
	require.Error(t, err, "a token whose aud does not include openrails must be rejected")
}

func TestDelegatedVerify_RejectsNonSelfPermission(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	// An operator/server-to-server grant on a browser token must be rejected.
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{
		Permissions: []string{PermAdmin},
	})
	_, _, err := v.VerifyDelegatedAccess(tok)
	require.Error(t, err)
}

func TestDelegatedVerify_RejectsMixedSelfAndOperatorPermission(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{
		Permissions: []string{PermSelfBillingRead, PermCreditsWrite},
	})
	_, _, err := v.VerifyDelegatedAccess(tok)
	require.Error(t, err, "any non-self permission taints the whole token")
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
			"tenant":        testDelegatedTenant,
			"delegated_sub": testDelegatedSubject,
			"permissions":   []string{PermSelfBillingRead},
			"iat":           past.Unix(),
			"exp":           past.Add(time.Minute).Unix(),
		},
		map[string]any{"typ": authhttp.DelegatedAccessTokenType},
	)
	require.NoError(t, err)
	_, _, verr := v.VerifyDelegatedAccess(tok)
	require.Error(t, verr, "expired delegated token must be rejected")
}

func TestDelegatedVerify_RejectsNoPermissions(t *testing.T) {
	v, signer := newTestDelegatedVerifier(t)
	tok := mintDelegated(t, signer, authhttp.DelegatedAccessParams{})
	_, _, err := v.VerifyDelegatedAccess(tok)
	require.Error(t, err, "a delegated token with no permissions is rejected")
}

// IsSelfPermission must accept exactly the four self-service permissions and
// reject operator/server-to-server grants.
func TestIsSelfPermission(t *testing.T) {
	for _, p := range SelfCatalogNames() {
		require.True(t, IsSelfPermission(p), p)
	}
	for _, p := range []string{PermAdmin, PermCreditsWrite, PermSubscriptionsCancel, "openrails:self:unknown", ""} {
		require.False(t, IsSelfPermission(p), p)
	}
}

// guard: ensure authcore error sentinels are wired for the resolver's mapping.
var _ = authcore.ErrAccessTokenExpired
