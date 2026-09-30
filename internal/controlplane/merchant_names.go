package controlplane

import (
	"context"
	"fmt"

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
