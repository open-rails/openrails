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
	"github.com/open-rails/openrails/internal/shared/moneyutil"
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
	valid := kind == "" || kind == billing.OfferPermanent && permanentPurchase(price) && (product.CreditGrant == nil || len(product.EntitlementsSpec) > 0) || kind == billing.OfferFinite && !price.AutoRenew && price.AccessDurationHours != nil || kind == billing.OfferRecurring && price.AutoRenew
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

// CheckoutPriceForAmount resolves a customer-selected deposit without mutating
// the catalog price. Fixed prices do not accept an amount override.
func CheckoutPriceForAmount(price *models.Price, amount *int64) (*models.Price, error) {
	if price == nil {
		return nil, fmt.Errorf("%w: price is required", ErrCheckoutAttemptValidation)
	}
	if price.CustomerAmount == nil {
		if amount != nil {
			return nil, fmt.Errorf("%w: amount is only allowed for customer_amount prices", ErrCheckoutAttemptValidation)
		}
		return price, nil
	}
	if amount == nil {
		return nil, fmt.Errorf("%w: amount is required for this price", ErrCheckoutAttemptValidation)
	}
	if price.AutoRenew || price.TrialUnitAmount != nil || price.TrialDurationHours != nil {
		return nil, fmt.Errorf("%w: customer_amount requires a one-time price without trial terms", ErrCheckoutAttemptValidation)
	}
	if price.CustomerAmount.MinAmount <= 0 || price.CustomerAmount.MaxAmount < price.CustomerAmount.MinAmount || *amount < price.CustomerAmount.MinAmount || *amount > price.CustomerAmount.MaxAmount {
		return nil, fmt.Errorf("%w: amount is outside this price's minimum and maximum", ErrCheckoutAttemptValidation)
	}
	if _, err := moneyutil.NativeToRailMinorExact(price.Currency, *amount); err != nil {
		return nil, fmt.Errorf("%w: amount must use whole currency minor units: %v", ErrCheckoutAttemptValidation, err)
	}
	selected := *price
	selected.Amount = *amount
	return &selected, nil
}
