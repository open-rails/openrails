package operator

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/merchants"
)

// SetMerchantAPIHost sets id's canonical #734 API host through the attached
// control plane (hosted products provision a merchant's
// api_host right after ProvisionMerchant succeeds, using the MerchantID that
// call already returned). apiHost empty clears the mapping. Mirrors
// ProvisionMerchant's shape: resolve the attached control plane, build the
// directory-only merchants.Service (merchants.NewDirectoryService), and
// forward to its SetHostConfig — the exact seam ProvisionMerchant already
// uses for the directory row itself. Safe to call multiple times (a plain
// UPDATE); returns merchants.ErrAPIHostTaken (errors.Is-able) when apiHost is
// already assigned to a different active merchant.
func SetMerchantAPIHost(ctx context.Context, cp *controlplane.ControlPlane, id billing.MerchantID, apiHost string) error {
	dir, err := merchants.NewDirectoryService(cp.Pool())
	if err != nil {
		return fmt.Errorf("control plane set host: build merchant directory service: %w", err)
	}
	if err := dir.SetHostConfig(ctx, id, apiHost); err != nil {
		return fmt.Errorf("control plane set host: %w", err)
	}
	return nil
}
