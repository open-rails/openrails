package models

import (
	"time"

	"github.com/google/uuid"
)

// Payment represents a payment event (both one-time and subscription payments)
// This is an immutable event log of all payments received
type Payment struct {
	ID uuid.UUID `json:"id"`
	// CustomerID is the OpenRails payable merchant subject for this row (#317).
	// Additive during the hard-cut rollout; writers populate it and readers move to
	// it before user_id is dropped. Join openrails.customers for issuer/subject.
	CustomerID uuid.UUID `json:"customer_id,omitempty"`
	PriceID    uuid.UUID `json:"price_id"`

	// Optional linkage to the subscription that generated this payment
	SubscriptionID *uuid.UUID `json:"subscription_id,omitempty"`

	// Optional linkage back to the payment that this record refunds
	RefundedPaymentID *uuid.UUID `json:"refunded_payment_id,omitempty"`

	Rail          Rail   `json:"rail"` // Rail: nmi, ccbill, solana
	TransactionID string `json:"transaction_id"`

	// Payment details - amount in MICROS (millionths of a major currency unit)
	Amount     int64  `json:"amount"`
	ListAmount int64  `json:"list_amount"`
	Currency   string `json:"currency"`
	Status     string `json:"status"`

	// RailMerchantAccountID is the provider account (openrails.rail_merchant_accounts.id)
	// that processed this charge (#641). Nil for legacy rows / unresolved accounts.
	RailMerchantAccountID *uuid.UUID `json:"rail_merchant_account_id,omitempty"`

	// Card snapshot of the payment method used for this charge, captured from
	// Stripe charge.succeeded / payment_method.attached webhooks. Immutable per
	// payment so history shows the card actually used even if the default later
	// changes. Never fetched from Stripe at query time.
	CardBrand *string `json:"card_brand,omitempty"`
	CardLast4 *string `json:"card_last4,omitempty"`

	DiscountCode     *string        `json:"discount_code,omitempty"`
	DiscountReason   *string        `json:"discount_reason,omitempty"`
	DiscountMetadata map[string]any `json:"discount_metadata,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`

	EntitlementsSpecSnapshot map[string]*int `json:"entitlements_spec_snapshot,omitempty"`
	CreditsSpecSnapshot      CreditsSpec     `json:"credits_spec_snapshot,omitempty"`

	PurchasedAt time.Time `json:"purchased_at"`
	CreatedAt   time.Time `json:"created_at"`

	// Relationships
	Price        *Price         `json:"price,omitempty"`
	Subscription *Subscription  `json:"subscription,omitempty"`
	Entitlements []*Entitlement `json:"entitlements,omitempty"`
}
