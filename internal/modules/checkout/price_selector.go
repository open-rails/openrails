package checkout

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

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

func validateOfferAssertion(price *models.Price, product *models.Product, key string, kind openrails.OfferKind) error {
	if key != "" {
		if strings.TrimSpace(key) == "" || len(key) > 256 || !utf8.ValidString(key) || strings.ContainsRune(key, 0) {
			return fmt.Errorf("%w: invalid entitlement", ErrCheckoutSessionValidation)
		}
		duration, ok := product.EntitlementsSpec[key]
		if !ok {
			return fmt.Errorf("%w: selected offer does not grant requested entitlement", ErrCheckoutSessionValidation)
		}
		if kind == openrails.OfferPermanent && duration != nil && *duration > 0 {
			return fmt.Errorf("%w: selected entitlement is not permanent", ErrCheckoutSessionValidation)
		}
	}
	valid := kind == "" || kind == openrails.OfferPermanent && permanentPurchase(price) || kind == openrails.OfferFinite && !price.AutoRenew && price.AccessDurationHours != nil || kind == openrails.OfferRecurring && price.AutoRenew
	if !valid {
		return fmt.Errorf("%w: selected price does not match requested offer kind", ErrCheckoutSessionValidation)
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
