//go:build integration

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

func TestNamingPolicyForwarding(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	disabled, zero, finite := false, time.Duration(0), 10*24*time.Hour
	for _, tc := range []struct {
		name     string
		input    authkit.NamingConfig
		override bool
	}{
		{name: "defaults"},
		{name: "disabled", input: authkit.NamingConfig{Enabled: &disabled}},
		{name: "zero forever", input: authkit.NamingConfig{RenameInterval: &zero, FormerNames: authkit.FormerNameRetentionConfig{Mode: authkit.FormerNamesForever}}},
		{name: "finite", input: authkit.NamingConfig{FormerNames: authkit.FormerNameRetentionConfig{Duration: &finite}}},
		{name: "attach override immediate", input: authkit.NamingConfig{RenameInterval: &zero, FormerNames: authkit.FormerNameRetentionConfig{Mode: authkit.FormerNamesImmediate}}, override: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sfx := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
			cfg := hostedTestConfig(t, dsn, "https://naming-"+sfx+".openrails.test")
			cfg.Auth.Naming = tc.input
			opts := embcp.AttachOptions{HostedPosture: true, EmailSender: &captureEmailSender{}}
			if tc.override {
				cfg.Auth.Naming = authkit.NamingConfig{Enabled: &disabled}
				opts.Naming = &tc.input
			}
			e := newHostApp(t, cfg)
			require.NoError(t, embcp.AttachWithOptions(ctx, e.App(), cfg, nil, opts))
			cp := embcp.Get(e.App())
			want, err := tc.input.Normalize()
			require.NoError(t, err)
			require.Equal(t, want, cp.Core().NamingPolicy())
			user, err := cp.Core().CreateUser(ctx, "naming-"+sfx+"@example.test", "naming"+sfx)
			require.NoError(t, err)
			original := "before-" + sfx
			res, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: original, OwnerUserID: user.ID})
			require.NoError(t, err)
			name := "after-" + sfx
			_, err = cp.Core().UpdateGroupInstanceAs(ctx, user.ID, res.GroupID, authkit.GroupInstanceUpdate{Slug: &name})
			if !want.Enabled {
				require.ErrorIs(t, err, authkit.ErrRenamesDisabled)
				return
			}
			require.NoError(t, err)
			state, err := cp.Core().GroupNamingState(ctx, res.GroupID)
			require.NoError(t, err)
			require.Equal(t, want, state.Policy)
			if want.FormerNameRetentionMode == authkit.FormerNamesImmediate {
				require.Empty(t, state.Aliases)
				_, _, err = cp.ResolveMerchantForGroup(ctx, original)
				require.ErrorIs(t, err, policy.ErrMerchantUnresolved)
			} else {
				require.Len(t, state.Aliases, 1)
				require.Equal(t, original, state.Aliases[0].Name)
				if want.FormerNameRetentionMode == authkit.FormerNamesForever {
					require.Nil(t, state.Aliases[0].ExpiresAt)
				} else {
					require.NotNil(t, state.NextRenameAt)
					require.Equal(t, state.NextRenameAt.Add(want.FormerNameRetention-want.RenameInterval), *state.Aliases[0].ExpiresAt)
				}
				mid, canonical, err := cp.ResolveMerchantForGroup(ctx, original)
				require.NoError(t, err)
				require.Equal(t, res.MerchantID, mid)
				require.Equal(t, name, canonical)
			}
			second := "again-" + sfx
			_, err = cp.Core().UpdateGroupInstanceAs(ctx, user.ID, res.GroupID, authkit.GroupInstanceUpdate{Slug: &second})
			if want.RenameInterval > 0 {
				require.ErrorIs(t, err, authkit.ErrRenameRateLimited)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCapturedMerchantGroupSurvivesNameReuse(t *testing.T) {
	ctx := context.Background()
	cfg := hostedTestConfig(t, dbtest.SharedPostgresDSN(t), "https://captured-group.openrails.test")
	zero := time.Duration(0)
	cfg.Auth.Naming = authkit.NamingConfig{RenameInterval: &zero, FormerNames: authkit.FormerNameRetentionConfig{Mode: authkit.FormerNamesImmediate}}
	e := newHostApp(t, cfg)
	refusedName := "refused-" + uuid.NewString()[:8]
	refusal := errors.New("host name rule")
	require.NoError(t, embcp.AttachWithOptions(ctx, e.App(), cfg, nil, embcp.AttachOptions{
		HostedPosture: true, EmailSender: &captureEmailSender{},
		MerchantCreation: &embcp.MerchantCreationConfig{ReservedSlugs: []string{"reserved-capture"}},
		NameAdmission: func(_ context.Context, req authkit.NameAdmissionRequest) error {
			if req.RequestedName == refusedName {
				return refusal
			}
			return nil
		},
	}))
	cp := embcp.Get(e.App())
	sfx := uuid.NewString()[:8]
	owner, err := cp.Core().CreateUser(ctx, "captured-"+sfx+"@example.test", "captured"+sfx)
	require.NoError(t, err)
	other, err := cp.Core().CreateUser(ctx, "claimant-"+sfx+"@example.test", "claimant"+sfx)
	require.NoError(t, err)
	teammate, err := cp.Core().CreateUser(ctx, "teammate-"+sfx+"@example.test", "teammate"+sfx)
	require.NoError(t, err)
	oldName, newName := "old-"+sfx, "new-"+sfx
	// Direct group creation is operator setup; the request mutations below are actor checked.
	gid, err := cp.Core().CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{Persona: "merchant", InstanceSlug: oldName, OwnerSubjectID: owner.ID})
	require.NoError(t, err)
	res, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: oldName})
	require.NoError(t, err)
	require.Equal(t, gid, res.GroupID)
	bound, err := cp.BindMerchantGroupContext(ctx, res.MerchantID, oldName)
	require.NoError(t, err)
	_, err = cp.Core().UpdateGroupInstanceAs(ctx, owner.ID, gid, authkit.GroupInstanceUpdate{Slug: &refusedName})
	require.ErrorIs(t, err, refusal)
	reserved := "reserved-capture"
	_, err = cp.Core().UpdateGroupInstanceAs(ctx, owner.ID, gid, authkit.GroupInstanceUpdate{Slug: &reserved})
	require.ErrorIs(t, err, authkit.ErrGroupSlugReserved)
	_, err = cp.Core().UpdateGroupInstanceAs(ctx, owner.ID, gid, authkit.GroupInstanceUpdate{Slug: &newName})
	require.NoError(t, err)
	replacement, err := cp.Core().CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{Persona: "merchant", InstanceSlug: oldName, OwnerSubjectID: other.ID})
	require.NoError(t, err)
	require.NotEqual(t, gid, replacement)
	require.NoError(t, cp.Core().AssignGroupRoleAs(bound, owner.ID, "merchant", oldName, teammate.ID, "user", "viewer"))
	team, err := cp.ListMerchantTeam(ctx, res.MerchantID)
	require.NoError(t, err)
	found := false
	for _, member := range team {
		found = found || member.UserID == teammate.ID
	}
	require.True(t, found, "the captured group receives the teammate after the spelling is reclaimed")
	allowed, err := cp.Core().CanOnGroup(ctx, teammate.ID, "user", replacement, "merchant:settings:read")
	require.NoError(t, err)
	require.False(t, allowed)
	key, secret, err := cp.MintMerchantAPIKey(ctx, res.MerchantID, "after rename", "viewer", owner.ID)
	require.NoError(t, err)
	require.NotEmpty(t, secret)
	keys, err := cp.ListMerchantAPIKeys(ctx, res.MerchantID)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	require.Equal(t, key.ID, keys[0].ID)
	revoked, err := cp.RevokeMerchantAPIKey(ctx, res.MerchantID, key.ID)
	require.NoError(t, err)
	require.True(t, revoked)
	inferred, canonical, err := cp.ResolveAuthorizedMerchant(ctx, "", owner.ID, "merchant:settings:read")
	require.NoError(t, err)
	require.Equal(t, res.MerchantID, inferred)
	require.Equal(t, newName, canonical)
	_, _, err = cp.ResolveAuthorizedMerchant(ctx, oldName, owner.ID, "merchant:settings:read")
	require.ErrorIs(t, err, policy.ErrPermissionRequired)
	// Customer handles represent immutable payer IDs; only their display names are mutable.
	customerGroup, err := cp.EnsureCustomerPermissionGroup(ctx, owner.ID, owner.ID)
	require.NoError(t, err)
	display := "Billing team"
	_, err = cp.Core().UpdateGroupInstanceAs(ctx, owner.ID, customerGroup, authkit.GroupInstanceUpdate{DisplayName: &display})
	require.NoError(t, err)
	_, err = cp.Core().UpdateGroupInstanceAs(ctx, owner.ID, customerGroup, authkit.GroupInstanceUpdate{Slug: &newName})
	require.ErrorIs(t, err, authkit.ErrGroupSlugApplicationManaged)
	// A previously captured request cannot fall through to the new owner after deletion.
	require.NoError(t, cp.Core().DeleteGroupInstanceByID(ctx, gid, authkit.DeletePermissionGroupOptions{ReleaseSlug: true}))
	err = cp.Core().AssignGroupRoleAs(bound, other.ID, "merchant", oldName, teammate.ID, "user", "viewer")
	require.ErrorIs(t, err, authkit.ErrGroupNotFound)
}
