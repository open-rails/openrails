package operator

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	"github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ApplyCatalog is a local operator operation against one explicitly selected
// merchant. Ordinary Client writes remain governed by AllowCatalogUpdates.
func (r *Operator) ApplyCatalog(ctx context.Context, merchantID merchant.ID, params *openrails.CatalogApplyParams) (*openrails.CatalogApplicationReceipt, error) {
	if err := r.initialized(); err != nil {
		return nil, err
	}
	if merchantID.IsZero() || params == nil {
		return nil, fmt.Errorf("merchant and catalog application are required")
	}
	svc, err := service.New(r.app.Runtime)
	if err != nil {
		return nil, err
	}
	ctx = catalogpolicy.OperatorContext(merchant.WithID(ctx, merchantID))
	return svc.ApplyCatalog(ctx, *params)
}
