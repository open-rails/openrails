package operator

import (
	"context"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/controlplane"
)

// ListActiveMerchantIDs returns one cursor page of active merchant IDs for an
// embedding host that must perform explicitly merchant-scoped background work.
// This is a privileged host seam; callers are responsible for authorizing and
// auditing its use.
func ListActiveMerchantIDs(ctx context.Context, cp *controlplane.ControlPlane, page billing.PageRequest) (*billing.ListPage[billing.MerchantID], error) {
	return cp.ListActiveMerchantIDs(ctx, page)
}
