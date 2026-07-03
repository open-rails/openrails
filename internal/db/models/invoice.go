package models

import (
	"time"

	"github.com/google/uuid"
)

// Invoice is a period bill/statement for a customer. In arrears billing, open
// invoices are receivables and payment state lives on the invoice. Prepaid
// invoices remain informational receipts/statements.
type Invoice struct {
	ID         uuid.UUID `json:"id"`
	MerchantID uuid.UUID `json:"merchant_id"`
	CustomerID uuid.UUID `json:"customer_id"`
	Currency   string    `json:"currency"`

	InvoiceNumber *string   `json:"invoice_number,omitempty"`
	PeriodFrom    time.Time `json:"period_from"`
	PeriodTo      time.Time `json:"period_to"`

	UsageTotal     int64 `json:"usage_total"`
	DepositsTotal  int64 `json:"deposits_total"`
	OwedAccrued    int64 `json:"owed_accrued"`
	OwedPaid       int64 `json:"owed_paid"`
	ClosingBalance int64 `json:"closing_balance"`
	SubtotalAmount int64 `json:"subtotal_amount"`
	TotalAmount    int64 `json:"total_amount"`
	AmountPaid     int64 `json:"amount_paid"`
	AmountDue      int64 `json:"amount_due"`

	// LineItems is the immutable as-billed statement itemization frozen at
	// close (#726): per-event_type usage rollups plus adjustment lines (e.g.
	// minimum_spend_trueup). MoneyMovements is the summed-by-type ledger snapshot.
	LineItems      []InvoiceLineItem `json:"line_items"`
	MoneyMovements map[string]int64  `json:"money_movements"`

	Status            string     `json:"status"`
	CollectionMethod  string     `json:"collection_method"`
	IssuedAt          *time.Time `json:"issued_at,omitempty"`
	DueAt             *time.Time `json:"due_at,omitempty"`
	PaidAt            *time.Time `json:"paid_at,omitempty"`
	VoidedAt          *time.Time `json:"voided_at,omitempty"`
	UncollectibleAt   *time.Time `json:"uncollectible_at,omitempty"`
	FinalizedAt       *time.Time `json:"finalized_at,omitempty"`
	ExternalInvoiceID *string    `json:"external_invoice_id,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// InvoiceLineItem is one statement line on an invoice: a per-event_type usage
// rollup (total amount, event count, summed dimensions) or an adjustment line
// (event_type = kind, e.g. "minimum_spend_trueup").
type InvoiceLineItem struct {
	EventType  string           `json:"event_type"`
	Amount     int64            `json:"amount"`
	Count      int64            `json:"count"`
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
}
