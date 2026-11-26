package payment

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

func validatePriceSelector(id, productKey, key string) error {
	if (strings.TrimSpace(id) == "") == (strings.TrimSpace(key) == "") {
		return fmt.Errorf("%w: exactly one of price_id or price_key is required", ErrInvoiceAttemptValidation)
	}
	if (strings.TrimSpace(productKey) != "") != (strings.TrimSpace(key) != "") {
		return fmt.Errorf("%w: product_key and price_key must be supplied together", ErrInvoiceAttemptValidation)
	}
	if id != "" {
		if parsed, err := billing.ParsePriceID(id); err != nil || parsed.IsZero() {
			return fmt.Errorf("%w: price_id must be a valid price ID; use price_key for an opaque key", ErrInvoiceAttemptValidation)
		}
	}
	return nil
}

func resolvePurchasePrice(ctx context.Context, prices *catalog.PriceService, id, productKey, key string) (*models.Price, error) {
	if err := validatePriceSelector(id, productKey, key); err != nil {
		return nil, err
	}
	if id != "" {
		parsed, _ := billing.ParsePriceID(id)
		return prices.GetByPublicID(ctx, parsed)
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return prices.GetCurrentByProductKey(ctx, mid.UUID(), productKey, key)
}
