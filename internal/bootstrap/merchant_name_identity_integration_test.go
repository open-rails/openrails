//go:build integration

package bootstrap

import (
	"context"
	"testing"

	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/stretchr/testify/require"
)

func TestManifestKeepsCapturedOwnerThroughNameReclaim(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	core := cp.Core()
	require.NoError(t, controlplane.EnsureRootContainment(ctx, core))
	owner, err := core.CreateUser(ctx, "manifest-owner@example.test", "manifest_owner")
	require.NoError(t, err)
	groupA, err := core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{Persona: controlplane.MerchantType, InstanceSlug: "manifest-former", OwnerSubjectID: owner.ID})
	require.NoError(t, err)
	directory, err := merchants.NewDirectoryService(cp.Pool())
	require.NoError(t, err)
	a, _, err := directory.Provision(ctx, merchants.ProvisionRequest{Slug: "manifest-former", PermissionGroupID: groupA})
	require.NoError(t, err)
	var b *merchants.Merchant
	directory.WithGroupSlugResolver(func(ctx context.Context, name string) (string, string, error) {
		captured, err := core.GroupInstanceForSlug(ctx, controlplane.MerchantType, name)
		require.NoError(t, err)
		current := "manifest-current"
		_, err = core.UpdateGroupInstanceAs(ctx, owner.ID, groupA, authkit.GroupInstanceUpdate{Slug: &current})
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `UPDATE profiles.name_claims SET expires_at=now()-interval '1 second' WHERE owner_id=$1::uuid AND name=$2`, groupA, name)
		require.NoError(t, err)
		groupB, err := core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{Persona: controlplane.MerchantType, InstanceSlug: name})
		require.NoError(t, err)
		b, _, err = directory.Provision(ctx, merchants.ProvisionRequest{Slug: name, PermissionGroupID: groupB})
		require.NoError(t, err)
		return captured.ID, captured.InstanceSlug, nil
	})
	selected, err := ProvisionMerchant(ctx, ProvisionMerchantRequest{
		Config: apiModeReconcileConfig(), ControlPlane: cp, Directory: directory,
		Slug: "manifest-former", Merchant: MerchantConfig{
			DisplayName:       "Original owner",
			RemoteApplication: &RemoteApplicationConfig{Slug: "manifest-owner-app", Issuer: "https://manifest-owner.test", JWKSURI: "https://manifest-owner.test/jwks.json"},
		}, Options: MerchantManifestReconcileOptions{Overwrite: true},
	})
	require.NoError(t, err)
	require.Equal(t, a.ID, selected.ID)
	require.NotEqual(t, a.ID, b.ID)
	var appID, appGroup string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text,permission_group_id::text FROM profiles.remote_applications WHERE slug=$1`, "manifest-owner-app").Scan(&appID, &appGroup))
	require.Equal(t, groupA, appGroup, "remote application remains nested under the captured owner")
	allowed, err := core.CanOnGroup(ctx, appID, authcore.SubjectKindRemoteApp, groupA, "merchant:*")
	require.NoError(t, err)
	require.True(t, allowed)
	allowed, err = core.CanOnGroup(ctx, appID, authcore.SubjectKindRemoteApp, b.PermissionGroupID, "merchant:*")
	require.NoError(t, err)
	require.False(t, allowed, "a reclaimed name cannot receive the old owner's application role")
	var displayA, displayB string
	require.NoError(t, pool.QueryRow(ctx, `SELECT coalesce(display_name,'') FROM openrails.merchants WHERE id=$1`, a.ID.UUID()).Scan(&displayA))
	require.NoError(t, pool.QueryRow(ctx, `SELECT coalesce(display_name,'') FROM openrails.merchants WHERE id=$1`, b.ID.UUID()).Scan(&displayB))
	require.Equal(t, "Original owner", displayA)
	require.Empty(t, displayB)
	// A subsequent name-bearing dump resolves the current claimant, while the
	// original owner's new name still exports its own configuration.
	original, err := DumpMerchantConfig(ctx, apiModeReconcileConfig(), cp, "manifest-current", DumpMerchantConfigOptions{})
	require.NoError(t, err)
	require.Equal(t, "Original owner", original.Merchants["manifest-current"].DisplayName)
	reclaimed, err := DumpMerchantConfig(ctx, apiModeReconcileConfig(), cp, "manifest-former", DumpMerchantConfigOptions{})
	require.NoError(t, err)
	require.Equal(t, "manifest-former", reclaimed.Merchants["manifest-former"].DisplayName)
}
