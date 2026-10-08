package checkout

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
)

// acceptedCreditGrant resolves an optional credit policy before provider work.
// It deliberately leaves dates unset: the promised lifetime begins when credit
// first becomes spendable, and settlement persists those dates once.
func acceptedCreditGrant(product *models.Product, price *models.Price) (*models.CreditGrantSnapshot, error) {
	if product.CreditGrant == nil {
		return nil, nil
	}
	if price.IsRecurring() {
		return nil, fmt.Errorf("recurring credit benefits are not supported")
	}
	policy := product.CreditGrant
	currency := strings.ToUpper(strings.TrimSpace(policy.Currency))
	if currency != strings.ToUpper(strings.TrimSpace(price.Currency)) {
		return nil, fmt.Errorf("purchased credit currency must match payment currency")
	}
	amount := price.Amount
	if policy.Amount != nil {
		amount = *policy.Amount
	} else if !policy.FromPayment {
		return nil, fmt.Errorf("credit benefit requires an amount or from_payment")
	}
	days := 365
	if policy.ExpiresAfterDays != nil {
		days = *policy.ExpiresAfterDays
	}
	out := &models.CreditGrantSnapshot{Amount: amount, Currency: currency, ExpiresAfterDays: days}
	return out, out.Validate()
}
