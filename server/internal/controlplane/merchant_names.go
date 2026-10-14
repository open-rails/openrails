package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchants"
)

// RenameMerchant renames an active merchant; its former name forwards to it
// under the naming policy. A merchant rename (operator=false) is subject to the
// reserved names, the creation pattern and the rename interval; actorUserID may
// hold the reserved-name escalation role. An operator rename is subject only to
// the former-name policy.
func (c *ControlPlane) RenameMerchant(ctx context.Context, mid billing.MerchantID, name, actorUserID string, operator bool) (*merchants.Merchant, error) {
	directory, err := c.directory()
	if err != nil {
		return nil, err
	}
	policy := c.naming
	if operator {
		policy.RenameInterval = 0
	} else if err := c.authorizeNameClaim(ctx, name, actorUserID); err != nil {
		return nil, err
	}
	return directory.Rename(ctx, mid, name, policy)
}

// SetMerchantDisplayName sets an active merchant's display name; an empty name
// is a no-op.
func (c *ControlPlane) SetMerchantDisplayName(ctx context.Context, id billing.MerchantID, displayName string) error {
	directory, err := c.directory()
	if err != nil {
		return err
	}
	return directory.SetDisplayName(ctx, id, displayName)
}

// ListUserMerchants returns the live merchants userID holds a role in,
// ordered by name, with the user's role there and what it grants: a declared
// role's from AuthKit's catalog, a custom role's from its merchant's group.
func (c *ControlPlane) ListUserMerchants(ctx context.Context, userID string) ([]billing.UserMerchant, error) {
	if c == nil || c.Core() == nil {
		return nil, ErrNoControlPlane
	}
	memberships, err := c.memberships(ctx, iam.UserSubject(strings.TrimSpace(userID)))
	if err != nil {
		return nil, err
	}
	roles := map[string]iam.Role{}
	var groups []string
	for _, m := range memberships {
		if m.Group.Persona == MerchantType {
			groups = append(groups, m.Group.ID)
			roles[m.Group.ID] = m.Role
		}
	}
	directory, err := c.directory()
	if err != nil {
		return nil, err
	}
	refs, err := directory.ListByGroups(ctx, groups)
	if err != nil {
		return nil, err
	}
	out := make([]billing.UserMerchant, 0, len(refs))
	for _, ref := range refs {
		role := roles[ref.GroupID]
		perms, err := c.roleGrants(ctx, ref.GroupID, role)
		switch {
		case errors.Is(err, iam.ErrRoleNotFound):
			continue // deleted since the listing: no longer held
		case err != nil:
			return nil, err
		}
		names := make([]string, len(perms))
		for i, p := range perms {
			names[i] = p.String()
		}
		out = append(out, billing.UserMerchant{ID: ref.ID, Slug: ref.Slug, DisplayName: ref.DisplayName, Role: role.Name(), Permissions: names})
	}
	return out, nil
}

// roleGrants is what role grants in group: a custom role is its group's.
func (c *ControlPlane) roleGrants(ctx context.Context, group string, role iam.Role) ([]iam.Perm, error) {
	if !isCustomRole(role.Name()) {
		return c.client.RolePermissions(role)
	}
	r, err := c.client.GroupRole(ctx, iam.GroupByID(group), role)
	return r.Grants, err
}
