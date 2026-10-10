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
// reads refunded or partially_refunded. A charge never fails: a decline is a
// PaymentAttempt. A refund the PSP refused is failed, as are decline records
// kept from before declines were attempts.
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
	// OrderID is the order the charge paid; its lines say what it bought.
	OrderID *OrderID `json:"order_id"`
	// InvoiceID is the invoice the payment paid.
	InvoiceID *InvoiceID `json:"invoice_id"`
	// PriceID, Price and Product are what a one-price charge bought; a
	// refund names its charge's. Null on an order's or invoice's payment.
	PriceID           *PriceID        `json:"price_id"`
	Price             *Price          `json:"price"`
	Product           *ProductSummary `json:"product"`
	Channel           PaymentChannel  `json:"channel"`
	Rail              *string         `json:"rail"`
	PSPID             *PSPID          `json:"psp_id"`
	TransactionID     string          `json:"transaction_id"`
	Card              *CardDetails    `json:"card"`
	Failure           *PaymentFailure `json:"failure"`
	RefundedPaymentID *PaymentID      `json:"refunded_payment_id"`
	// Reason is the merchant's stated reason for a refund.
	Reason *string `json:"reason"`
	// Refunds are the reversals of a charge: read with a single payment, null
	// in lists.
	Refunds   []Payment `json:"refunds"`
	CreatedAt time.Time `json:"created_at"`
}

// PaymentListParams selects payments, newest first; every filter is
// optional. IDs instead reads 1 to MaxBatchItems named payments in one page,
// whatever their state; unknown ones are absent.
type PaymentListParams struct {
	IDs            []PaymentID
	CustomerID     CustomerID
	SubscriptionID SubscriptionID
	InvoiceID      InvoiceID
	OrderID        OrderID
	Status         PaymentStatus
	Rail           string
	Kind           PaymentKind
	TransactionID  string
	PageRequest
}

// CreatePaymentParams records money the merchant received outside OpenRails
// (cash, a bank transfer) for one invoice or one order; it moves no money.
// An invoice takes up to what it has due; an order takes its total, and one
// with a recurring line is refused, since its renewals charge the customer's
// card. TransactionID is the remittance's identity: recording it again with
// the same terms answers the first payment, with other terms is
// idempotency_key_reused. PaidAt defaults to now.
type CreatePaymentParams struct {
	InvoiceID     *InvoiceID `json:"invoice_id,omitempty"`
	OrderID       *OrderID   `json:"order_id,omitempty"`
	Amount        int64      `json:"amount,string"`
	TransactionID string     `json:"transaction_id"`
	PaidAt        *time.Time `json:"paid_at,omitempty"`
}
