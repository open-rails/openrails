package controlplane

import (
	"context"
	"errors"
	"strings"

	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"

	"github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// MerchantGroupRef addresses a merchant's AuthKit group by its immutable id
// (#1106): AuthKit's group names are not OpenRails merchant names. The context
// binds the reference to that id, so AuthKit never resolves it as a name, and
// every use still rechecks that the group is live.
func MerchantGroupRef(ctx context.Context, groupID string) (context.Context, authkit.GroupRef) {
	groupID = strings.ToLower(strings.TrimSpace(groupID))
	ctx = authcore.WithResolvedGroup(ctx, authkit.GroupInstance{ID: groupID, Persona: MerchantType}, groupID)
	return ctx, authkit.GroupRef{Persona: MerchantType, Instance: groupID}
}

// merchantGroupScopeForID addresses the AuthKit group bound to an active
// merchant.
func (c *ControlPlane) merchantGroupScopeForID(ctx context.Context, mid merchant.ID) (context.Context, authkit.GroupRef, error) {
	if c == nil || c.Core() == nil {
		return ctx, authkit.GroupRef{}, ErrNoControlPlane
	}
	directory, err := c.directory()
	if err != nil {
		return ctx, authkit.GroupRef{}, err
	}
	row, err := directory.Get(ctx, mid)
	if errors.Is(err, merchants.ErrMerchantNotFound) {
		return ctx, authkit.GroupRef{}, policy.ErrMerchantUnresolved
	}
	if err != nil {
		return ctx, authkit.GroupRef{}, err
	}
	if row.PermissionGroupID == "" || row.Status != merchants.StatusActive {
		return ctx, authkit.GroupRef{}, policy.ErrMerchantUnresolved
	}
	ctx, ref := MerchantGroupRef(ctx, row.PermissionGroupID)
	return ctx, ref, nil
}

// directory is the merchant directory over the control plane's pool.
func (c *ControlPlane) directory() (*merchants.Service, error) {
	if c == nil || c.pool == nil {
		return nil, ErrNoControlPlane
	}
	return merchants.NewDirectoryService(c.pool)
}
