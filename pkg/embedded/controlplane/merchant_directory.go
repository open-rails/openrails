package controlplane

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ListActiveMerchantIDs returns one page of active merchant IDs for an
// embedding host that must perform explicitly merchant-scoped background work.
// This is a privileged host seam; callers are responsible for authorizing and
// auditing its use.
func ListActiveMerchantIDs(ctx context.Context, a *app.App, limit, offset int) ([]merchant.ID, error) {
	cp := Get(a)
	if cp == nil {
		return nil, fmt.Errorf("control plane: no control plane attached (call Attach first)")
	}
	return cp.ListActiveMerchantIDs(ctx, limit, offset)
}
