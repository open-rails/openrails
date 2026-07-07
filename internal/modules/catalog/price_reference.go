package catalog

import (
	"context"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ResolveReference resolves a caller-supplied price identifier that may be
// EITHER a price UUID (bare or "price_"-prefixed, api.ParsePriceID's format)
// OR a #774 price_key. Every checkout/API entrypoint that historically
// accepted only a price id now accepts a price_key at the SAME field: try the
// id parse first (api.ParsePriceID is total over its own format, so a real
// key string never collides with it), fall back to a key lookup only when
// that fails.
func ResolveReference(ctx context.Context, prices *PriceService, ref string) (*models.Price, error) {
	if id, err := api.ParsePriceID(ref); err == nil {
		return prices.GetByID(ctx, id)
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return prices.GetCurrentByKey(ctx, tid.UUID(), ref)
}

// ResolveReferenceForMerchant is ResolveReference for callers that already
// have the merchant id in hand (no ambient request context to Require it
// from) — e.g. batch/background jobs.
func ResolveReferenceForMerchant(ctx context.Context, prices *PriceService, merchantID uuid.UUID, ref string) (*models.Price, error) {
	if id, err := api.ParsePriceID(ref); err == nil {
		return prices.GetByID(ctx, id)
	}
	return prices.GetCurrentByKey(ctx, merchantID, ref)
}
