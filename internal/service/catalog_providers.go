package service

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// OpenRails writes no provider catalog: our prices are billed by OpenRails,
// and provider prices are read from their provider. The only object it
// publishes for a price is the on-chain Solana plan a recurring price of ours
// sells on.

// errRemoteWritesDisabled: the merchant's write posture (limited or readonly)
// blocks publishing a Solana plan.
var errRemoteWritesDisabled = errors.New("provider writes are held by the merchant's write posture")

// ErrPriceNotSellable refuses a price none of its methods can sell.
var ErrPriceNotSellable = apperr.New(http.StatusBadRequest, billing.CodePriceNotSellable, "no PSP can sell this price")

// solanaPlanContext is the price a Solana plan is published for.
type solanaPlanContext struct {
	PriceID              uuid.UUID
	Product              *models.Product
	UnitAmount           int64
	Currency             string
	BillingCycleDays     *int
	BillingIntervalHours *int
}
