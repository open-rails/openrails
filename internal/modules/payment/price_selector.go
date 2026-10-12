package payment

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

func validatePriceSelector(id, productKey, key string) error {
	if (strings.TrimSpace(id) == "") == (strings.TrimSpace(key) == "") {
		return fmt.Errorf("%w: exactly one of price_id or price_key is required", ErrOrderAttemptValidation)
	}
	if (strings.TrimSpace(productKey) != "") != (strings.TrimSpace(key) != "") {
		return fmt.Errorf("%w: product_key and price_key must be supplied together", ErrOrderAttemptValidation)
	}
	if id != "" {
		if parsed, err := billing.ParsePriceID(id); err != nil || parsed.IsZero() {
			return fmt.Errorf("%w: price_id must be a valid price ID; use price_key for an opaque key", ErrOrderAttemptValidation)
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

// PurchasePriceForAmount resolves a customer-selected deposit without mutating
// the catalog price. Fixed prices do not accept an amount override.
func PurchasePriceForAmount(price *models.Price, amount *int64) (*models.Price, error) {
	if price == nil {
		return nil, fmt.Errorf("%w: price is required", ErrOrderAttemptValidation)
	}
	if price.CustomerAmount == nil {
		if amount != nil {
			return nil, fmt.Errorf("%w: amount is only allowed for customer_amount prices", ErrOrderAttemptValidation)
		}
		return price, nil
	}
	if amount == nil {
		return nil, fmt.Errorf("%w: amount is required for this price", ErrOrderAttemptValidation)
	}
	if price.IsRecurring() || price.TrialUnitAmount != nil || price.TrialDurationHours != nil {
		return nil, fmt.Errorf("%w: customer_amount requires a one-time price without trial terms", ErrOrderAttemptValidation)
	}
	if price.CustomerAmount.MinAmount <= 0 || price.CustomerAmount.MaxAmount < price.CustomerAmount.MinAmount || *amount < price.CustomerAmount.MinAmount || *amount > price.CustomerAmount.MaxAmount {
		return nil, fmt.Errorf("%w: amount is outside this price's minimum and maximum", ErrOrderAttemptValidation)
	}
	if _, err := moneyutil.NativeToRailMinorExact(price.Currency, *amount); err != nil {
		return nil, fmt.Errorf("%w: amount must use whole currency minor units: %v", ErrOrderAttemptValidation, err)
	}
	selected := *price
	selected.Amount = *amount
	return &selected, nil
}
