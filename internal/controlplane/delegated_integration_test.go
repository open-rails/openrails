//go:build integration

package controlplane

import (
	"context"
	"github.com/open-rails/authkit/verify"
	"github.com/open-rails/openrails/internal/testauth"
	"net/http"
	"testing"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/jwtkit"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestDelegatedStoredAuthorityWorkflow(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedSuperuserPGXPool(t)
	cp := newTestControlPlane(t, pool)
	const target = "https://receiver.openrails.test/v1/me/status"
	verify.WithDPoP(cp.Core().ClaimDPoPProof, func(*http.Request) string { return target })(cp.DelegatedVerifier())
	owner, err := cp.Core().CreateUser(ctx, "delegation-owner@example.test", "delegation-owner")
	require.NoError(t, err)
	// Revoking the app must leave the merchant's human recovery owner intact.
	seed, err := cp.Bootstrap(ctx, BootstrapOptions{BootstrapMerchantSlug: dbtest.TestMerchantSlug, InitialAdminUserID: owner.ID})
	require.NoError(t, err)
	signer, err := jwtkit.NewRSASigner(2048, testDelegatedKID)
	require.NoError(t, err)
	mint := func(params authkit.DelegatedAccessParams) string {
		params.Issuer = testDelegatedIssuer
		params.DelegatedSubject = testDelegatedSubject
		if len(params.Audiences) == 0 {
			params.Audiences = []string{canonicalAudience}
		}
		token, err := testauth.MintDelegated(ctx, signer, params)
		require.NoError(t, err)
		return token
	}
	app, err := cp.Core().UpsertRemoteApplication(ctx, authkit.RemoteApplication{
		Issuer: testDelegatedIssuer, Slug: "delegation-workflow", Enabled: true,
		Mode: authkit.RemoteAppModeStatic, Tier: authkit.ApplicationTierApproved,
		PermissionGroupID: seed.BootstrapMerchantGroupID,
		PublicKeys:        []authkit.RemoteAppKey{{KID: signer.KID(), PublicKeyPEM: testPublicKeyPEM(t, signer.PublicKey())}},
	})
	require.NoError(t, err)
	require.NoError(t, cp.Core().Genesis().AssignRemoteApplicationRole(ctx, app.ID, "owner"))
	require.NoError(t, cp.ReloadRemoteApplications(ctx))
	for _, permissions := range [][]string{nil, {PermMerchantAdmissionsCreate}} {
		token := mint(authkit.DelegatedAccessParams{Permissions: permissions})
		principal, err := cp.ResolveDelegated(testauth.Request(ctx, http.MethodGet, target, token))
		require.NoError(t, err)
		require.Equal(t, testDelegatedSubject, principal.DelegatedSubject)
		require.Equal(t, dbtest.TestMerchantSlug, principal.MerchantSlug)
		require.ElementsMatch(t, permissions, principal.Permissions)
	}
	for _, params := range []authkit.DelegatedAccessParams{
		{Permissions: []string{"root:*"}},
		{Audiences: []string{wrongAudience}},
	} {
		_, err := cp.ResolveDelegated(testauth.Request(ctx, http.MethodGet, target, mint(params)))
		require.ErrorIs(t, err, ErrDelegatedInvalid)
	}
	token := mint(authkit.DelegatedAccessParams{})
	app.Enabled = false
	_, err = cp.Core().UpsertRemoteApplication(ctx, *app)
	require.NoError(t, err)
	_, err = cp.ResolveDelegated(testauth.Request(ctx, http.MethodGet, target, token))
	require.Error(t, err, "revocation takes effect without a registry reload")
}
