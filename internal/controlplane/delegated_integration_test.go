//go:build integration

package controlplane

import (
	"context"
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
	seed, err := cp.Bootstrap(ctx, BootstrapOptions{BootstrapMerchantSlug: dbtest.TestMerchantSlug})
	require.NoError(t, err)
	signer, err := jwtkit.NewRSASigner(2048, testDelegatedKID)
	require.NoError(t, err)
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
		token := mintDelegated(t, signer, authkit.DelegatedAccessParams{Permissions: permissions})
		principal, err := cp.ResolveDelegated(ctx, token, "")
		require.NoError(t, err)
		require.Equal(t, testDelegatedSubject, principal.DelegatedSubject)
		require.Equal(t, dbtest.TestMerchantSlug, principal.MerchantSlug)
		require.ElementsMatch(t, permissions, principal.Permissions)
	}
	for _, params := range []authkit.DelegatedAccessParams{
		{Permissions: []string{"root:*"}},
		{Audiences: []string{wrongAudience}},
	} {
		_, err := cp.ResolveDelegated(ctx, mintDelegated(t, signer, params), "")
		require.ErrorIs(t, err, ErrDelegatedInvalid)
	}
	token := mintDelegated(t, signer, authkit.DelegatedAccessParams{})
	app.Enabled = false
	_, err = cp.Core().UpsertRemoteApplication(ctx, *app)
	require.NoError(t, err)
	_, err = cp.ResolveDelegated(ctx, token, "")
	require.Error(t, err, "revocation takes effect without a registry reload")
}
