package payments

import (
	"time"

	"github.com/google/uuid"
)

type RegisterPurchaseRequest struct {
	UserID           string
	PriceID          uuid.UUID
	Rail             string
	TransactionID    string
	Amount           int64
	AmountProvided   bool
	Currency         string
	SubscriptionID   *uuid.UUID
	WalletPurchase   bool
	PurchasedAt      *time.Time
	DiscountCode     *string
	DiscountReason   *string
	DiscountMetadata map[string]any
	Metadata         map[string]any
	// AttemptKind stamps payments.attempt_kind (initial|renewal, #733);
	// empty = unknown (manual/imported rows).
	AttemptKind string
	// TokenType stamps payments.token_type (charge.TokenType* consts, #796);
	// empty = unknown (non-card rails, imported rows).
	TokenType string
}

type RegisterPurchaseResponse struct {
	PaymentID    uuid.UUID
	Entitlements []string
	DelayedStart *time.Time
	Eligibility  string
}
