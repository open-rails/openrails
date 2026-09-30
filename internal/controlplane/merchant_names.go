package controlplane

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// merchantNamingPolicy applies the site naming policy (auth.naming, the policy
// AuthKit applies to usernames) to OpenRails merchant names (#1106).
func merchantNamingPolicy(cfg authkit.NamingConfig) (merchants.NamingPolicy, error) {
	p, err := cfg.Normalize()
	if err != nil {
		return merchants.NamingPolicy{}, fmt.Errorf("controlplane: merchant naming policy: %w", err)
	}
	return merchants.NamingPolicy{
		Enabled:             p.Enabled,
		RenameInterval:      p.RenameInterval,
		FormerNames:         merchants.FormerNames(p.FormerNameRetentionMode),
		FormerNameRetention: p.FormerNameRetention,
	}, nil
}

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

// UserMerchant is a live merchant a user holds a role in.
type UserMerchant struct {
	ID          merchant.ID `json:"id"`
	Slug        string      `json:"slug"`
	DisplayName string      `json:"display_name,omitempty"`
	Role        string      `json:"role"`
}

// ListUserMerchants returns the live merchants userID holds a role in, ordered
// by name, with the user's highest role in each.
func (c *ControlPlane) ListUserMerchants(ctx context.Context, userID string) ([]UserMerchant, error) {
	if c == nil || c.Core() == nil {
		return nil, ErrNoControlPlane
	}
	memberships, err := c.Core().ListSubjectGroups(ctx, authkit.UserSubject(strings.TrimSpace(userID)))
	if err != nil {
		return nil, err
	}
	roles := map[string]string{}
	var groups []string
	for _, m := range memberships {
		if m.Persona != MerchantType {
			continue
		}
		role, seen := roles[m.GroupID]
		if !seen {
			groups = append(groups, m.GroupID)
		}
		if !seen || teamRoleRank(string(m.Role)) < teamRoleRank(role) {
			roles[m.GroupID] = string(m.Role)
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
	out := make([]UserMerchant, 0, len(refs))
	for _, ref := range refs {
		out = append(out, UserMerchant{ID: ref.ID, Slug: ref.Slug, DisplayName: ref.DisplayName, Role: roles[ref.GroupID]})
	}
	return out, nil
}
