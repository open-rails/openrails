package payment

import (
	"fmt"

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
	return models.AcceptCreditGrant(product.CreditGrant, price.Currency, price.Amount)
}
