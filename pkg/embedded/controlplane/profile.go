package controlplane

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrMerchantNotFound indicates that no active merchant matched the requested
// directory update.
var ErrMerchantNotFound = merchants.ErrMerchantNotFound

// SetMerchantDisplayName sets an active merchant's human-readable directory
// name through the attached control plane. Empty names are ignored so hosts can
// safely retry provisioning without clearing a previously stored name. This is
// a privileged host seam; callers must authorize the merchant ID before use.
func SetMerchantDisplayName(ctx context.Context, a *app.App, id merchant.ID, displayName string) error {
	cp := Get(a)
	if cp == nil {
		return fmt.Errorf("control plane set merchant display name: no control plane attached (call Attach first)")
	}
	dir, err := merchants.NewDirectoryService(cp.Pool())
	if err != nil {
		return fmt.Errorf("control plane set merchant display name: build merchant directory service: %w", err)
	}
	if err := dir.SetDisplayName(ctx, id, displayName); err != nil {
		return fmt.Errorf("control plane set merchant display name: %w", err)
	}
	return nil
}
