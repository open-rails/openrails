package models

import (
	"time"

	"github.com/google/uuid"
)

// MoneyMovement is the positive marker that replaced the settlement feed's
// transaction_id denylist (or#827). The zero value is UNDECLARED, which is
// distinct from MoneyMovementNone: a writer that never thought about it must
// not pass for one that decided no money moved.
type MoneyMovement string

const (
	// MoneyMovementUndeclared is the zero value: nobody said. Rejected at
	// insert for rows that would otherwise be settlement candidates.
	MoneyMovementUndeclared MoneyMovement = ""
	// MoneyMovementRail — money actually moved at the payment rail and
	// transaction_id is the rail's own reference. The host feed publishes
	// exactly these.
	MoneyMovementRail MoneyMovement = "rail"
	// MoneyMovementNone — bookkeeping row: an attempt anchor whose real
	// charge is a different row, a decline, a placeholder.
	MoneyMovementNone MoneyMovement = "none"
)

// Valid reports whether m is a declared, storable value.
func (m MoneyMovement) Valid() bool {
	return m == MoneyMovementRail || m == MoneyMovementNone
}

// Payment represents a payment event (both one-time and subscription payments)
// This is an immutable event log of all payments received
type Payment struct {
	ID uuid.UUID `json:"id"`
	// CustomerID is the OpenRails payable merchant subject for this row (#317).
	// Join openrails.customers for issuer/subject.
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

	// PspID is the PSP (openrails.psps.id)
	// that processed this charge (#641). Nil for legacy rows / unresolved accounts.
	PspID *uuid.UUID `json:"psp_id,omitempty"`

	// Card snapshot of the payment method used for this charge, captured from
	// Stripe charge.succeeded / payment_method.attached webhooks. Immutable per
	// payment so history shows the card actually used even if the default later
	// changes. Never fetched from Stripe at query time.
	CardBrand *string `json:"card_brand,omitempty"`
	CardLast4 *string `json:"card_last4,omitempty"`

	// AttemptKind: initial|renewal, stamped at write time (#733). Nil = unknown
	// (imported / pre-instrumentation rows).
	AttemptKind *string `json:"attempt_kind,omitempty"`
	// FailureCode is the raw rail decline code, verbatim; FailureReason the
	// normalized category derived deterministically per rail (#733).
	FailureCode   *string `json:"failure_code,omitempty"`
	FailureReason *string `json:"failure_reason,omitempty"`
	// ReversalKind discriminates mirror rows: refund|chargeback|dispute_reversal (#733).
	ReversalKind *string `json:"reversal_kind,omitempty"`
	// TokenType is the credential form presented at charge time (#796):
	// network_token|pan_via_proxy|psp_token. Nil = unknown/legacy.
	TokenType *string `json:"token_type,omitempty"`

	// MoneyMovement declares whether this row records money that actually
	// moved at the rail (or#827). It is the ONLY thing the host settlement
	// feed keys on, so it is a required declaration on any completed
	// positive charge — see paymentInsertParams.
	MoneyMovement MoneyMovement `json:"money_movement,omitempty"`

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
