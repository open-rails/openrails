//go:build integration

package controlplane_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

func TestProvisionRepairKeepsCapturedIdentity(t *testing.T) {
	ctx := context.Background()
	cfg := hostedTestConfig(t, dbtest.SharedPostgresDSN(t), "https://repair.openrails.test")
	zero := time.Duration(0)
	cfg.Auth.Naming = authkit.NamingConfig{RenameInterval: &zero, FormerNames: authkit.FormerNameRetentionConfig{Mode: authkit.FormerNamesImmediate}}
	e := newHostApp(t, cfg)
	var duringAdmission func(context.Context) error
	require.NoError(t, embcp.AttachWithOptions(ctx, e.App(), cfg, nil, embcp.AttachOptions{
		MerchantCreation: &embcp.MerchantCreationConfig{Admission: func(ctx context.Context, _, _ string) error {
			if duringAdmission != nil {
				return duringAdmission(ctx)
			}
			return nil
		}},
	}))
	cp := embcp.Get(e.App())
	core := cp.Core()
	suffix := uuid.NewString()[:8]
	owner, err := core.CreateUser(ctx, "repair-"+suffix+"@example.test", "repair"+suffix)
	require.NoError(t, err)
	other, err := core.CreateUser(ctx, "other-"+suffix+"@example.test", "other"+suffix)
	require.NoError(t, err)
	oldName, newName := "before-"+suffix, "after-"+suffix
	original, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: oldName, OwnerUserID: owner.ID})
	require.NoError(t, err)

	// A host admission callback can run after the initial lookup. It must not
	// turn an existing group into a new creation when the old name is released.
	duringAdmission = func(ctx context.Context) error {
		_, err := core.UpdateGroupInstanceAs(ctx, owner.ID, original.GroupID, authkit.GroupInstanceUpdate{Slug: &newName})
		return err
	}
	repaired, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: oldName, OwnerUserID: owner.ID})
	require.NoError(t, err)
	require.Equal(t, original.MerchantID, repaired.MerchantID)
	require.False(t, repaired.Created)
	duringAdmission = nil

	replacement, err := core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{Persona: "merchant", InstanceSlug: oldName, OwnerSubjectID: other.ID})
	require.NoError(t, err)
	repaired, err = embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: oldName, OwnerUserID: owner.ID, ExistingGroupID: original.GroupID})
	require.NoError(t, err)
	require.Equal(t, original.MerchantID, repaired.MerchantID)
	require.Equal(t, original.GroupID, repaired.GroupID)
	require.False(t, repaired.Created)
	_, err = embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: oldName, OwnerUserID: other.ID, ExistingGroupID: original.GroupID})
	require.ErrorIs(t, err, authkit.ErrInsufficientRoleAuthority)

	// Recover the existing group's missing billing attachment without creating
	// another group or changing the meaning of Created (the billing insert).
	missingName := "missing-" + suffix
	missingID, err := core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{Persona: "merchant", InstanceSlug: missingName, OwnerSubjectID: owner.ID})
	require.NoError(t, err)
	attached, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{OwnerUserID: owner.ID, ExistingGroupID: missingID})
	require.NoError(t, err)
	require.True(t, attached.Created)
	require.Equal(t, missingID, attached.GroupID)
	again, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{OwnerUserID: owner.ID, ExistingGroupID: missingID})
	require.NoError(t, err)
	require.False(t, again.Created)
	require.Equal(t, attached.MerchantID, again.MerchantID)

	_, err = cp.Pool().Exec(ctx, `UPDATE openrails.merchants SET deleted_at=now() WHERE id=$1`, attached.MerchantID.UUID())
	require.NoError(t, err)
	_, err = embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{OwnerUserID: owner.ID, ExistingGroupID: missingID})
	require.ErrorIs(t, err, merchants.ErrMerchantNotFound)

	customerID, err := core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{Persona: "customer", InstanceSlug: owner.ID, OwnerSubjectID: owner.ID})
	require.NoError(t, err)
	_, err = embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: oldName, OwnerUserID: owner.ID, ExistingGroupID: customerID})
	require.ErrorIs(t, err, authkit.ErrInsufficientRoleAuthority)
	require.NoError(t, core.DeleteGroupInstanceByID(ctx, original.GroupID, authkit.DeletePermissionGroupOptions{ReleaseSlug: true}))
	_, err = embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: oldName, OwnerUserID: owner.ID, ExistingGroupID: original.GroupID})
	require.ErrorIs(t, err, authkit.ErrGroupNotFound)
	group, err := core.GroupInstanceByID(ctx, replacement)
	require.NoError(t, err)
	allowed, err := core.CanOnGroup(ctx, authkit.UserSubject(other.ID), group.ID, "merchant:*")
	require.NoError(t, err)
	require.True(t, allowed)
}
