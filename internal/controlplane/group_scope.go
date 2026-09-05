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

func (c *ControlPlane) merchantGroupIdentity(ctx context.Context, mid merchant.ID) (authkit.GroupInstance, error) {
	if c == nil || c.Core() == nil {
		return authkit.GroupInstance{}, ErrNoControlPlane
	}
	directory, err := merchants.NewDirectoryService(c.pool)
	if err != nil {
		return authkit.GroupInstance{}, err
	}
	row, err := directory.Get(ctx, mid)
	if errors.Is(err, merchants.ErrMerchantNotFound) {
		return authkit.GroupInstance{}, policy.ErrMerchantUnresolved
	}
	if err != nil {
		return authkit.GroupInstance{}, err
	}
	if row.PermissionGroupID == "" || row.Status != merchants.StatusActive {
		return authkit.GroupInstance{}, policy.ErrMerchantUnresolved
	}
	group, err := c.Core().GroupInstanceByID(ctx, row.PermissionGroupID)
	if errors.Is(err, authkit.ErrGroupNotFound) {
		return authkit.GroupInstance{}, policy.ErrMerchantUnresolved
	}
	if err != nil {
		return authkit.GroupInstance{}, err
	}
	if group.Persona != MerchantType || group.InstanceSlug == "" {
		return authkit.GroupInstance{}, policy.ErrMerchantUnresolved
	}
	return group, nil
}

// BindMerchantGroupContext carries an already authorized billing identity into
// name-addressed AuthKit operations. It grants no permission; core mutations
// still enforce their actor checks against this captured group UUID.
func (c *ControlPlane) BindMerchantGroupContext(ctx context.Context, mid merchant.ID, reference string) (context.Context, error) {
	group, err := c.merchantGroupIdentity(ctx, mid)
	if err != nil {
		return ctx, err
	}
	if strings.TrimSpace(reference) == "" {
		return ctx, policy.ErrMerchantUnresolved
	}
	return authcore.WithResolvedGroup(ctx, group, reference), nil
}

func (c *ControlPlane) merchantGroupScopeForID(ctx context.Context, mid merchant.ID) (context.Context, string, error) {
	group, err := c.merchantGroupIdentity(ctx, mid)
	if err != nil {
		return ctx, "", err
	}
	return authcore.WithResolvedGroup(ctx, group, group.InstanceSlug), group.InstanceSlug, nil
}
