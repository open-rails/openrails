package controlplane

import (
	"context"

	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/operator"
)

type ProvisionMerchantForRestoreRequest = operator.ProvisionMerchantForRestoreRequest

// ErrMerchantRestoreConflict indicates a destination UUID, name or group is
// already assigned differently. Existing authority is never replaced.
var ErrMerchantRestoreConflict = merchants.ErrMerchantRestoreConflict

// ProvisionMerchantForRestore preserves the archive's billing UUID while
// binding it to an existing destination group authorized by its current owner.
// It imports no AuthKit identity or permission from the source deployment.
func (c *ControlPlane) ProvisionMerchantForRestore(ctx context.Context, req ProvisionMerchantForRestoreRequest) (*ProvisionMerchantResult, error) {
	return operator.ProvisionMerchantForRestore(ctx, c.app, req)
}
