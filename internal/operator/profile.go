package operator

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/merchants"
)

// ErrMerchantNotFound indicates that no active merchant matched the requested
// directory update.
var ErrMerchantNotFound = merchants.ErrMerchantNotFound

// ListUserMerchants returns the live merchants userID holds a role in, with its
// highest role in each: the "my merchants" read. This is a privileged host
// seam; callers pass the authenticated user.
func ListUserMerchants(ctx context.Context, a *app.App, userID string) ([]billing.UserMerchant, error) {
	cp := Get(a)
	if cp == nil {
		return nil, fmt.Errorf("control plane list user merchants: no control plane attached (call Attach first)")
	}
	return cp.ListUserMerchants(ctx, userID)
}

// SetMerchantDisplayName sets an active merchant's human-readable directory
// name through the attached control plane. Empty names are ignored so hosts can
// safely retry provisioning without clearing a previously stored name. This is
// a privileged host seam; callers must authorize the merchant ID before use.
func SetMerchantDisplayName(ctx context.Context, a *app.App, id billing.MerchantID, displayName string) error {
	cp := Get(a)
	if cp == nil {
		return fmt.Errorf("control plane set merchant display name: no control plane attached (call Attach first)")
	}
	if err := cp.SetMerchantDisplayName(ctx, id, displayName); err != nil {
		return fmt.Errorf("control plane set merchant display name: %w", err)
	}
	return nil
}

// RenameMerchant renames an active merchant as the operator: no reserved-name
// or rename-interval check, and the former name forwards to it under the site
// naming policy. This is a privileged host seam; callers must authorize it.
func RenameMerchant(ctx context.Context, a *app.App, id billing.MerchantID, name string) error {
	cp := Get(a)
	if cp == nil {
		return fmt.Errorf("control plane rename merchant: no control plane attached (call Attach first)")
	}
	if _, err := cp.RenameMerchant(ctx, id, name, "", true); err != nil {
		return fmt.Errorf("control plane rename merchant: %w", err)
	}
	return nil
}
