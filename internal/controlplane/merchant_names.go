package controlplane

import (
	"context"
	"strings"

	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// RenameMerchant renames an active merchant; its former name forwards to it
// under the naming policy. A merchant rename (operator=false) is subject to the
// reserved names, the creation pattern and the rename interval; actorUserID may
// hold the reserved-name escalation role. An operator rename is subject only to
// the former-name policy.
func (c *ControlPlane) RenameMerchant(ctx context.Context, mid merchant.ID, name, actorUserID string, operator bool) (*merchants.Merchant, error) {
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
func (c *ControlPlane) SetMerchantDisplayName(ctx context.Context, id merchant.ID, displayName string) error {
	directory, err := c.directory()
	if err != nil {
		return err
	}
	return directory.SetDisplayName(ctx, id, displayName)
}

// ListUserMerchants returns the live merchants userID holds a role in, ordered
// by name, with the user's role in each.
func (c *ControlPlane) ListUserMerchants(ctx context.Context, userID string) ([]billing.UserMerchant, error) {
	if c == nil || c.Core() == nil {
		return nil, ErrNoControlPlane
	}
	memberships, err := c.memberships(ctx, iam.UserSubject(strings.TrimSpace(userID)))
	if err != nil {
		return nil, err
	}
	roles := map[string]string{}
	var groups []string
	for _, m := range memberships {
		if m.Group.Persona != MerchantType {
			continue
		}
		groups = append(groups, m.Group.ID)
		roles[m.Group.ID] = m.Role.Name()
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
		out = append(out, billing.UserMerchant{ID: ref.ID, Slug: ref.Slug, DisplayName: ref.DisplayName, Role: roles[ref.GroupID]})
	}
	return out, nil
}
