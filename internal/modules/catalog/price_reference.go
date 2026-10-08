package catalog

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

// ResolveReference resolves a caller-supplied price reference that is EITHER
// a typed price id ("price_<uuid>") OR a #774 price_key: the id spelling never
// collides with a key, so an id parse is tried first and a key lookup follows
// only when the reference is not an id.
func ResolveReference(ctx context.Context, prices *PriceService, productKey, ref string) (*models.Price, error) {
	if id, err := billing.ParsePriceID(ref); productKey == "" && err == nil && !id.IsZero() {
		return prices.GetByID(ctx, id.UUID())
	}
	if productKey == "" {
		return nil, fmt.Errorf("product_key is required with a price key")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return prices.GetCurrentByProductKey(ctx, tid.UUID(), productKey, ref)
}
