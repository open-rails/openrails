//go:build integration

package controlplane_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestControlPlaneProvisionsRestoreIdentityUnderDestinationAuthority(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{
		Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI,
		SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: dbtest.SharedPostgresDSN(t)},
		Auth: &config.AuthConfig{Issuer: "https://restore.openrails.test", KeysPath: t.TempDir()},
	}
	rt, err := embed.New(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	cp, err := controlplane.Attach(ctx, rt, controlplane.Options{})
	require.NoError(t, err)
	suffix := uuid.NewString()[:8]
	owner, err := cp.Core().CreateUser(ctx, "restore-"+suffix+"@example.test", "restore"+suffix)
	require.NoError(t, err)
	other, err := cp.Core().CreateUser(ctx, "other-"+suffix+"@example.test", "other"+suffix)
	require.NoError(t, err)
	// Normal provisioning still allocates its own UUID and establishes root containment.
	normal, err := cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: "normal-" + suffix, OwnerUserID: owner.ID})
	require.NoError(t, err)
	require.True(t, normal.Created)
	normalAgain, err := cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: "normal-" + suffix, OwnerUserID: owner.ID})
	require.NoError(t, err)
	require.False(t, normalAgain.Created)
	require.Equal(t, normal.MerchantID, normalAgain.MerchantID)

	group, err := cp.Core().CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{
		Persona: controlplane.MerchantType, InstanceSlug: "destination-" + suffix,
		ParentPersona: authkit.RootPersona, OwnerSubjectID: owner.ID,
	})
	require.NoError(t, err)
	id := merchant.ID(uuid.New())
	req := controlplane.ProvisionMerchantForRestoreRequest{MerchantID: id, ExistingGroupID: group, OwnerUserID: owner.ID}
	wrongOwner := req
	wrongOwner.OwnerUserID = other.ID
	_, err = cp.ProvisionMerchantForRestore(ctx, wrongOwner)
	require.ErrorIs(t, err, authkit.ErrInsufficientRoleAuthority)
	_, err = cp.ProvisionMerchantForRestore(ctx, controlplane.ProvisionMerchantForRestoreRequest{ExistingGroupID: group, OwnerUserID: owner.ID})
	require.ErrorContains(t, err, "merchant_id is required")
	first, err := cp.ProvisionMerchantForRestore(ctx, req)
	require.NoError(t, err)
	require.True(t, first.Created)
	require.Equal(t, id, first.MerchantID)
	require.Equal(t, group, first.GroupID)
	again, err := cp.ProvisionMerchantForRestore(ctx, req)
	require.NoError(t, err)
	require.False(t, again.Created)
	require.Equal(t, first.MerchantID, again.MerchantID)
	resolved, canonical, err := cp.ResolveAuthorizedMerchant(ctx, "destination-"+suffix, owner.ID, "merchant:settings:read")
	require.NoError(t, err)
	require.Equal(t, id, resolved)
	require.Equal(t, "destination-"+suffix, canonical)

	wrongGroup := req
	wrongGroup.ExistingGroupID = normal.GroupID
	_, err = cp.ProvisionMerchantForRestore(ctx, wrongGroup)
	require.ErrorIs(t, err, controlplane.ErrMerchantRestoreConflict)
	wrongID := req
	wrongID.MerchantID = normal.MerchantID
	_, err = cp.ProvisionMerchantForRestore(ctx, wrongID)
	require.ErrorIs(t, err, controlplane.ErrMerchantRestoreConflict)
	wrongID.MerchantID = merchant.ID(uuid.New())
	_, err = cp.ProvisionMerchantForRestore(ctx, wrongID)
	require.ErrorIs(t, err, controlplane.ErrMerchantRestoreConflict)
	_, err = rt.RegisterMerchantForRestore(ctx, id, canonical)
	require.ErrorContains(t, err, "attached control plane")

	customerGroup, err := cp.Core().CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{
		Persona: controlplane.CustomerType, InstanceSlug: owner.ID, OwnerSubjectID: owner.ID,
	})
	require.NoError(t, err)
	wrongGroup.ExistingGroupID = customerGroup
	_, err = cp.ProvisionMerchantForRestore(ctx, wrongGroup)
	require.ErrorIs(t, err, authkit.ErrInsufficientRoleAuthority)
	host, err := cp.GetMerchantAPIHost(ctx, id)
	require.NoError(t, err)
	require.Empty(t, host)
	allowed, err := cp.Core().CanOnGroup(ctx, authkit.UserSubject(other.ID), group, controlplane.MerchantType.OwnerGrant())
	require.NoError(t, err)
	require.False(t, allowed, "provisioning never grants authority from the source")
}
