package billing

import (
	"time"
)

// PaymentKind is what a payment row records: a charge, or a reversal of one.
type PaymentKind string

const (
	PaymentCharge          PaymentKind = "charge"
	PaymentRefund          PaymentKind = "refund"
	PaymentChargeback      PaymentKind = "chargeback"
	PaymentDisputeReversal PaymentKind = "dispute_reversal"
)

// PaymentStatus is where a payment stands. A charge with refunds against it
// reads refunded or partially_refunded.
type PaymentStatus string

const (
	PaymentPending           PaymentStatus = "pending"
	PaymentSucceeded         PaymentStatus = "succeeded"
	PaymentFailed            PaymentStatus = "failed"
	PaymentRefunded          PaymentStatus = "refunded"
	PaymentPartiallyRefunded PaymentStatus = "partially_refunded"
)

// PaymentChannel is how money reached the merchant: through a PSP on a rail,
// or recorded by the merchant outside any rail.
type PaymentChannel string

const (
	ChannelRail   PaymentChannel = "rail"
	ChannelManual PaymentChannel = "manual"
	ChannelAdmin  PaymentChannel = "admin"
)

// Payment is money that moved, or a reversal of it. A declined authorization
// is a PaymentAttempt, never a Payment. Amount is native units of Currency,
// positive for a charge or dispute reversal, negative for a refund or
// chargeback. Rail and PSPID are null off-rail.
type Payment struct {
	ID             PaymentID       `json:"id"`
	Kind           PaymentKind     `json:"kind"`
	Status         PaymentStatus   `json:"status"`
	Amount         int64           `json:"amount,string"`
	AmountRefunded int64           `json:"amount_refunded,string"`
	Currency       string          `json:"currency"`
	CustomerID     CustomerID      `json:"customer_id"`
	SubscriptionID *SubscriptionID `json:"subscription_id"`
	PriceID        PriceID         `json:"price_id"`
	// Price and Product are what the charge bought; a refund names its
	// charge's.
	Price             *SubscriptionPrice `json:"price"`
	Product           *ProductSummary    `json:"product"`
	Channel           PaymentChannel     `json:"channel"`
	Rail              *string            `json:"rail"`
	PSPID             *string            `json:"psp_id"`
	TransactionID     string             `json:"transaction_id"`
	Card              *CardDetails       `json:"card"`
	Failure           *PaymentFailure    `json:"failure"`
	RefundedPaymentID *PaymentID         `json:"refunded_payment_id"`
	// Reason is the merchant's stated reason for a refund.
	Reason *string `json:"reason"`
	// Refunds are the reversals of a charge: read with a single payment, null
	// in lists.
	Refunds   []Payment `json:"refunds"`
	CreatedAt time.Time `json:"created_at"`
}

// ListPaymentsParams selects payments, newest first; every filter is
// optional.
type ListPaymentsParams struct {
	CustomerID     CustomerID
	SubscriptionID SubscriptionID
	PriceID        PriceID
	Rail           string
	Kind           PaymentKind
	TransactionID  string
	Page           PageRequest
}

// PaymentSettlementStatus reports whether a customer has ever paid for a
// price through a rail. Refunding or archiving the payment does not undo it.
type PaymentSettlementStatus struct {
	Settled bool `json:"settled"`
}

// CreateOffChannelPaymentParams records a purchase paid outside any rail
// (cash, bank transfer): the customer gets what the price grants.
// TransactionID is the remittance's identity: recording it again with the
// same terms answers the first record, with other terms is
// idempotency_key_reused. Amount defaults to the price's amount.
type CreateOffChannelPaymentParams struct {
	PriceID          PriceID        `json:"price_id"`
	TransactionID    string         `json:"transaction_id"`
	Amount           *int64         `json:"amount,omitempty,string"`
	Currency         string         `json:"currency,omitempty"`
	PurchasedAt      *time.Time     `json:"purchased_at,omitempty"`
	DiscountCode     *string        `json:"discount_code,omitempty"`
	DiscountReason   *string        `json:"discount_reason,omitempty"`
	DiscountMetadata map[string]any `json:"discount_metadata,omitempty"`
}
