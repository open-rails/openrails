package controlplane

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// SetMerchantAPIHost sets id's canonical #734 API host through the attached
// control plane (openrails-saas #14: hosted products provision a merchant's
// api_host right after ProvisionMerchant succeeds, using the MerchantID that
// call already returned). apiHost empty clears the mapping. Mirrors
// ProvisionMerchant's shape: resolve the attached control plane, build the
// directory-only merchants.Service (merchants.NewDirectoryService), and
// forward to its SetHostConfig — the exact seam ProvisionMerchant already
// uses for the directory row itself. Safe to call multiple times (a plain
// UPDATE); returns merchants.ErrAPIHostTaken (errors.Is-able) when apiHost is
// already assigned to a different active merchant.
func SetMerchantAPIHost(ctx context.Context, a *app.App, id merchant.ID, apiHost string) error {
	cp := Get(a)
	if cp == nil {
		return fmt.Errorf("control plane set host: no control plane attached (call Attach first)")
	}
	dir, err := merchants.NewDirectoryService(cp.Pool())
	if err != nil {
		return fmt.Errorf("control plane set host: build merchant directory service: %w", err)
	}
	if err := dir.SetHostConfig(ctx, id, apiHost); err != nil {
		return fmt.Errorf("control plane set host: %w", err)
	}
	return nil
}

// GetMerchantAPIHost returns id's current #734 API host (empty when unset)
// through the attached control plane.
func GetMerchantAPIHost(ctx context.Context, a *app.App, id merchant.ID) (string, error) {
	cp := Get(a)
	if cp == nil {
		return "", fmt.Errorf("control plane get host: no control plane attached (call Attach first)")
	}
	dir, err := merchants.NewDirectoryService(cp.Pool())
	if err != nil {
		return "", fmt.Errorf("control plane get host: build merchant directory service: %w", err)
	}
	cfg, err := dir.GetHostConfig(ctx, id)
	if err != nil {
		return "", fmt.Errorf("control plane get host: %w", err)
	}
	return cfg.APIHost, nil
}
