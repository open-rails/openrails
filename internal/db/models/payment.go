package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// Payment represents a payment event (both one-time and subscription payments)
// This is an immutable event log of all payments received
type Payment struct {
	bun.BaseModel `bun:"table:billing.payments,alias:purch"`

	ID      uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	UserID  string    `bun:"user_id,notnull" json:"user_id"`
	PriceID uuid.UUID `bun:"price_id,notnull" json:"price_id"`

	// Optional linkage to the subscription that generated this payment
	SubscriptionID *uuid.UUID `bun:"subscription_id,type:uuid,nullzero" json:"subscription_id,omitempty"`

	// Optional linkage back to the payment that this record refunds
	RefundedPaymentID *uuid.UUID `bun:"refunded_payment_id,type:uuid,nullzero" json:"refunded_payment_id,omitempty"`

	Processor     Processor `bun:"processor,notnull" json:"processor"` // Processor: mobius, ccbill, solana
	TransactionID string    `bun:"transaction_id,notnull" json:"transaction_id"`

	// Payment details - amount in cents (smallest currency unit)
	Amount     int64  `bun:"amount,notnull" json:"amount"`
	ListAmount int64  `bun:"list_amount,notnull" json:"list_amount"`
	Currency   string `bun:"currency,notnull,default:'usd'" json:"currency"`

	DiscountCode     *string        `bun:"discount_code,nullzero" json:"discount_code,omitempty"`
	DiscountReason   *string        `bun:"discount_reason,nullzero" json:"discount_reason,omitempty"`
	DiscountMetadata map[string]any `bun:"discount_metadata,type:jsonb,nullzero" json:"discount_metadata,omitempty"`
	Metadata         map[string]any `bun:"metadata,type:jsonb,nullzero" json:"metadata,omitempty"`

	PurchasedAt time.Time `bun:"purchased_at,notnull,default:current_timestamp" json:"purchased_at"`
	CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`

	// Relationships
	Price        *Price         `bun:"rel:belongs-to,join:price_id=id" json:"price,omitempty"`
	Subscription *Subscription  `bun:"rel:belongs-to,join:subscription_id=id" json:"subscription,omitempty"`
	Entitlements []*Entitlement `bun:"rel:has-many,join:id=source_id" json:"entitlements,omitempty"`
}
