package checkout

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

func validateCheckoutPriceSelector(id, productKey, key string) error {
	if (strings.TrimSpace(id) == "") == (strings.TrimSpace(key) == "") {
		return fmt.Errorf("%w: exactly one of price_id or price_key is required", ErrCheckoutAttemptValidation)
	}
	if (strings.TrimSpace(productKey) != "") != (strings.TrimSpace(key) != "") {
		return fmt.Errorf("%w: product_key and price_key must be supplied together", ErrCheckoutAttemptValidation)
	}
	if id != "" {
		if parsed, err := billing.ParsePriceID(id); err != nil || parsed.IsZero() {
			return fmt.Errorf("%w: price_id must be a valid price ID; use price_key for an opaque key", ErrCheckoutAttemptValidation)
		}
	}
	return nil
}

func validateOfferAssertion(price *models.Price, product *models.Product, key string, kind billing.OfferKind) error {
	if key != "" {
		if strings.TrimSpace(key) == "" || len(key) > 256 || !utf8.ValidString(key) || strings.ContainsRune(key, 0) {
			return fmt.Errorf("%w: invalid entitlement", ErrCheckoutAttemptValidation)
		}
		duration, ok := product.EntitlementsSpec[key]
		if !ok {
			return fmt.Errorf("%w: selected offer does not grant requested entitlement", ErrCheckoutAttemptValidation)
		}
		if kind == billing.OfferPermanent && duration != nil && *duration > 0 {
			return fmt.Errorf("%w: selected entitlement is not permanent", ErrCheckoutAttemptValidation)
		}
	}
	valid := kind == "" || kind == billing.OfferPermanent && permanentPurchase(price) || kind == billing.OfferFinite && !price.AutoRenew && price.AccessDurationHours != nil || kind == billing.OfferRecurring && price.AutoRenew
	if !valid {
		return fmt.Errorf("%w: selected price does not match requested offer kind", ErrCheckoutAttemptValidation)
	}
	return nil
}

func resolveCheckoutPrice(ctx context.Context, prices *catalog.PriceService, id, productKey, key string) (*models.Price, error) {
	if err := validateCheckoutPriceSelector(id, productKey, key); err != nil {
		return nil, err
	}
	if id != "" {
		parsed, _ := billing.ParsePriceID(id)
		return prices.GetByID(ctx, parsed.UUID())
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return prices.GetCurrentByProductKey(ctx, mid.UUID(), productKey, key)
}
