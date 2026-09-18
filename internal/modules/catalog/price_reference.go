package catalog

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ResolveReference resolves a caller-supplied price reference that is EITHER
// a typed price id ("price_<uuid>") OR a #774 price_key: the id spelling never
// collides with a key, so an id parse is tried first and a key lookup follows
// only when the reference is not an id.
func ResolveReference(ctx context.Context, prices *PriceService, ref string) (*models.Price, error) {
	if id, err := openrails.ParsePriceID(ref); err == nil && !id.IsZero() {
		return prices.GetByID(ctx, id.UUID())
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return prices.GetCurrentByKey(ctx, tid.UUID(), ref)
}
