package controlplane

import (
	"context"
	"crypto"
	"errors"
	"testing"
	"time"

	authhttp "github.com/open-rails/authkit/http"
	jwtkit "github.com/open-rails/authkit/jwt"
	"github.com/stretchr/testify/require"
)

// These tests pin the SECURITY-CRITICAL mint decisions of the browser-tier
// delegated-token MINT side (issue #222): the minted token round-trips through
// the EXACT verifier configuration the /v1/self surface uses (same key, issuer,
// audience, self-permission catalog), the requested tenant/subject/permissions are
// honored, non-self permissions are refused, and the TTL is clamped. They build a
// minimal ControlPlane wired with a freshly generated signing key — the same key
// the test verifier trusts — so no database is needed (the tenant resolution in
// ResolveDelegated is covered by the service token/middleware tests).

// newMintControlPlane builds a ControlPlane whose active signer + the returned
// verifier share one generated key, mirroring how New wires the real deployment.
func newMintControlPlane(t *testing.T) (*ControlPlane, *authhttp.Verifier) {
	t.Helper()
	signer, err := jwtkit.NewRSASigner(2048, testDelegatedKID)
	require.NoError(t, err)

	cp := &ControlPlane{
		signer:             signer,
		issuer:             testDelegatedIssuer,
		delegatedAudiences: []string{canonicalAudience},
	}

	v := authhttp.NewVerifier(
		authhttp.WithTenantMode("multi"),
		authhttp.WithPermissionCatalog(func(permissions []string) error {
			if len(permissions) == 0 {
				return errors.New("delegated token carries no permissions")
			}
			for _, p := range permissions {
				if !IsSelfPermission(p) {
					return errors.New("permission not in self catalog: " + p)
				}
			}
			return nil
		}),
	)
	require.NoError(t, v.AddIssuer(testDelegatedIssuer, []string{canonicalAudience}, authhttp.IssuerOptions{
		RawKeys: map[string]crypto.PublicKey{testDelegatedKID: signer.PublicKey()},
	}))
	return cp, v
}

// (1) An service token-authed mint returns a token the EXISTING /v1/self delegated verifier
// accepts for the same tenant + delegated_sub.
func TestMintDelegated_RoundTripsThroughSelfVerifier(t *testing.T) {
	cp, v := newMintControlPlane(t)

	minted, err := cp.MintDelegatedAccessToken(context.Background(), MintDelegatedParams{
		Tenant:           testDelegatedTenant,
		DelegatedSubject: testDelegatedSubject,
		Permissions:      []string{PermSelfBillingRead, PermSelfCheckoutCreate},
	})
	require.NoError(t, err)
	require.NotEmpty(t, minted.Token)

	cl, dp, err := v.VerifyDelegatedAccess(minted.Token)
	require.NoError(t, err, "the existing /v1/self verifier must accept the minted token")
	require.Equal(t, "", cl.UserID, "minted token must not carry a normal sub")
	require.Equal(t, testDelegatedSubject, dp.DelegatedSubject)
	require.Equal(t, testDelegatedTenant, dp.Tenant)
	require.Contains(t, dp.Permissions, PermSelfBillingRead)
	require.Contains(t, dp.Permissions, PermSelfCheckoutCreate)
}

// (2) Requesting a non-self / operator permission is rejected.
func TestMintDelegated_RejectsNonSelfPermission(t *testing.T) {
	cp, _ := newMintControlPlane(t)

	_, err := cp.MintDelegatedAccessToken(context.Background(), MintDelegatedParams{
		Tenant:           testDelegatedTenant,
		DelegatedSubject: testDelegatedSubject,
		Permissions:      []string{PermSelfBillingRead, PermCreditsWrite},
	})
	var forbidden *ErrMintForbiddenPermission
	require.ErrorAs(t, err, &forbidden, "an operator permission taints the whole mint")
	require.Equal(t, PermCreditsWrite, forbidden.Permission)
}

func TestMintDelegated_RejectsMintPermissionItself(t *testing.T) {
	cp, _ := newMintControlPlane(t)
	// The mint capability is NOT a self permission: a browser token can never
	// carry it, so requesting it must be refused.
	_, err := cp.MintDelegatedAccessToken(context.Background(), MintDelegatedParams{
		Tenant:           testDelegatedTenant,
		DelegatedSubject: testDelegatedSubject,
		Permissions:      []string{PermSelfMint},
	})
	var forbidden *ErrMintForbiddenPermission
	require.ErrorAs(t, err, &forbidden)
}

// (3) The minted token's tenant always equals the param tenant (the handler binds
// it to the service token's tenant; here we assert mint honors exactly what it is given and
// never derives tenant from elsewhere).
func TestMintDelegated_TenantIsExactlyTheParam(t *testing.T) {
	cp, v := newMintControlPlane(t)

	minted, err := cp.MintDelegatedAccessToken(context.Background(), MintDelegatedParams{
		Tenant:           testDelegatedTenant,
		DelegatedSubject: testDelegatedSubject,
		Permissions:      []string{PermSelfBillingRead},
	})
	require.NoError(t, err)
	_, dp, err := v.VerifyDelegatedAccess(minted.Token)
	require.NoError(t, err)
	require.Equal(t, testDelegatedTenant, dp.Tenant)
}

func TestMintDelegated_RejectsEmptyTenant(t *testing.T) {
	cp, _ := newMintControlPlane(t)
	_, err := cp.MintDelegatedAccessToken(context.Background(), MintDelegatedParams{
		Tenant:           "   ",
		DelegatedSubject: testDelegatedSubject,
		Permissions:      []string{PermSelfBillingRead},
	})
	require.ErrorIs(t, err, ErrServiceTokenTenantUnresolved)
}

func TestMintDelegated_RejectsEmptySubject(t *testing.T) {
	cp, _ := newMintControlPlane(t)
	_, err := cp.MintDelegatedAccessToken(context.Background(), MintDelegatedParams{
		Tenant:      testDelegatedTenant,
		Permissions: []string{PermSelfBillingRead},
	})
	require.ErrorIs(t, err, ErrMintNoSubject)
}

func TestMintDelegated_RejectsNoPermissions(t *testing.T) {
	cp, _ := newMintControlPlane(t)
	_, err := cp.MintDelegatedAccessToken(context.Background(), MintDelegatedParams{
		Tenant:           testDelegatedTenant,
		DelegatedSubject: testDelegatedSubject,
		Permissions:      []string{"  "},
	})
	require.ErrorIs(t, err, ErrMintNoPermissions)
}

// (4) TTL is clamped: a non-positive TTL defaults, an over-cap TTL is capped, and
// an in-range TTL is honored.
func TestMintDelegated_ClampsTTL(t *testing.T) {
	cp, _ := newMintControlPlane(t)

	cases := []struct {
		name      string
		requested time.Duration
		wantExp   time.Duration
	}{
		{"default-when-zero", 0, MintDefaultTTL},
		{"default-when-negative", -time.Minute, MintDefaultTTL},
		{"capped-when-over-max", time.Hour, MintMaxTTL},
		{"honored-when-in-range", 2 * time.Minute, 2 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := time.Now()
			minted, err := cp.MintDelegatedAccessToken(context.Background(), MintDelegatedParams{
				Tenant:           testDelegatedTenant,
				DelegatedSubject: testDelegatedSubject,
				Permissions:      []string{PermSelfBillingRead},
				TTL:              tc.requested,
			})
			require.NoError(t, err)
			gotTTL := minted.ExpiresAt.Sub(before)
			require.InDelta(t, tc.wantExp.Seconds(), gotTTL.Seconds(), 5,
				"expires_at should reflect the clamped TTL")
		})
	}
}

func TestMintDelegated_NotConfigured(t *testing.T) {
	cp := &ControlPlane{} // no signer/issuer
	_, err := cp.MintDelegatedAccessToken(context.Background(), MintDelegatedParams{
		Tenant:           testDelegatedTenant,
		DelegatedSubject: testDelegatedSubject,
		Permissions:      []string{PermSelfBillingRead},
	})
	require.ErrorIs(t, err, ErrMintNotConfigured)
}

// The mint permission is in the operator catalog (granted to the operator role)
// but is NOT a browser self permission.
func TestMintPermission_InOperatorCatalogNotSelfCatalog(t *testing.T) {
	require.Contains(t, CatalogNames(), PermSelfMint)
	require.Contains(t, OperatorRolePermissions(), PermSelfMint)
	require.False(t, IsSelfPermission(PermSelfMint))
	require.NotContains(t, SelfCatalogNames(), PermSelfMint)
}
