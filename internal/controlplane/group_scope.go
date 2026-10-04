package controlplane

import (
	"context"
	"errors"

	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchants"
)

// merchantGroup addresses the AuthKit group bound to an active merchant.
func (c *ControlPlane) merchantGroup(ctx context.Context, mid billing.MerchantID) (iam.GroupRef, error) {
	if c == nil || c.Core() == nil {
		return iam.GroupRef{}, ErrNoControlPlane
	}
	directory, err := c.directory()
	if err != nil {
		return iam.GroupRef{}, err
	}
	row, err := directory.Get(ctx, mid)
	if errors.Is(err, merchants.ErrMerchantNotFound) {
		return iam.GroupRef{}, billing.ErrMerchantUnresolved
	}
	if err != nil {
		return iam.GroupRef{}, err
	}
	if row.PermissionGroupID == "" || row.Status != merchants.StatusActive {
		return iam.GroupRef{}, billing.ErrMerchantUnresolved
	}
	return iam.GroupByID(row.PermissionGroupID), nil
}

// directory is the merchant directory over the control plane's pool.
func (c *ControlPlane) directory() (*merchants.Service, error) {
	if c == nil || c.pool == nil {
		return nil, ErrNoControlPlane
	}
	return merchants.NewDirectoryService(c.pool)
}
