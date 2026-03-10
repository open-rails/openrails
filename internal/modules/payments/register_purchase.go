package payments

import (
	"time"

	"github.com/google/uuid"
)

type RegisterPurchaseRequest struct {
	UserID           string
	PriceID          uuid.UUID
	Processor        string
	TransactionID    string
	Amount           int64
	Currency         string
	SubscriptionID   *uuid.UUID
	WalletPurchase   bool
	PurchasedAt      *time.Time
	DiscountCode     *string
	DiscountReason   *string
	DiscountMetadata map[string]any
	Metadata         map[string]any
}
