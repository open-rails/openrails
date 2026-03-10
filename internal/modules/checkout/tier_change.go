package checkout

import (
	"errors"

	"github.com/google/uuid"
)

type TierChangeRequest struct {
	PriceID        string    `json:"price_id"`
	SubscriptionID uuid.UUID `json:"-"`
	IdempotencyKey string    `json:"-"`
}

var (
	ErrTierChangeNoSubscription = errors.New("no active subscription found")
	ErrTierChangeNotSupported   = errors.New("tier change not supported for this processor")
	ErrTierChangeBlocked        = errors.New("tier change blocked")
	ErrTierChangePending        = errors.New("tier change already pending")
	ErrTierChangeSameProduct    = errors.New("already on this plan")
	ErrTierChangeDifferentGroup = errors.New("cannot change to a different tier group")
)

type TierChangeError struct {
	HTTPStatus int
	Message    string
	Code       string
}

func (e *TierChangeError) Error() string {
	return e.Message
}
