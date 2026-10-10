package operator

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/server/internal/controlplane"
)

// SetMerchantAPIHost binds id's canonical API host without proof; "" clears
// it. Idempotent; merchants.ErrAPIHostTaken when another active merchant
// holds the host.
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
