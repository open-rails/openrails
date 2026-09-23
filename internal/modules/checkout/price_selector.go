package checkout

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
)

func validateCheckoutPriceSelector(id, key string) error {
	if (strings.TrimSpace(id) == "") == (strings.TrimSpace(key) == "") {
		return fmt.Errorf("%w: exactly one of price_id or price_key is required", ErrCheckoutSessionValidation)
	}
	if id != "" {
		if parsed, err := openrails.ParsePriceID(id); err != nil || parsed.IsZero() {
			return fmt.Errorf("%w: price_id must be a valid price ID; use price_key for an opaque key", ErrCheckoutSessionValidation)
		}
	}
	return nil
}

func resolveCheckoutPrice(ctx context.Context, prices *catalog.PriceService, id, key string) (*models.Price, error) {
	if err := validateCheckoutPriceSelector(id, key); err != nil {
		return nil, err
	}
	if id != "" {
		parsed, _ := openrails.ParsePriceID(id)
		return prices.GetByID(ctx, parsed.UUID())
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return prices.GetCurrentByKey(ctx, mid.UUID(), key)
}
