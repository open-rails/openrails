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

	InvoiceNumber  *string   `json:"invoice_number,omitempty"`
	PeriodStartsAt time.Time `json:"period_starts_at"`
	PeriodEndsAt   time.Time `json:"period_ends_at"`

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
	// close: per-event_type usage rollups. MoneyMovements is the ledger snapshot.
	LineItems      []InvoiceLineItem `json:"line_items"`
	MoneyMovements map[string]int64  `json:"money_movements"`

	// Enterprise document fields, snapshotted from the payer's
	// customer_invoice_profiles row at finalize. Tax is a host-defined shape.
	PONumber        *string          `json:"po_number,omitempty"`
	Tax             map[string]any   `json:"tax,omitempty"`
	BillingContacts []InvoiceContact `json:"billing_contacts,omitempty"`
	Memo            *string          `json:"memo,omitempty"`

	Status                       string     `json:"status"`
	CollectionMethod             string     `json:"collection_method"`
	IssuedAt                     *time.Time `json:"issued_at,omitempty"`
	DueAt                        *time.Time `json:"due_at,omitempty"`
	PaidAt                       *time.Time `json:"paid_at,omitempty"`
	VoidedAt                     *time.Time `json:"voided_at,omitempty"`
	UncollectibleAt              *time.Time `json:"uncollectible_at,omitempty"`
	FinalizedAt                  *time.Time `json:"finalized_at,omitempty"`
	ExternalInvoiceID            *string    `json:"external_invoice_id,omitempty"`
	CollectionFailureCount       int32      `json:"collection_failure_count"`
	CollectionAttemptCount       int32      `json:"collection_attempt_count"`
	CollectionFailedAt           *time.Time `json:"collection_failed_at,omitempty"`
	NextCollectionAttemptAt      *time.Time `json:"next_collection_attempt_at,omitempty"`
	LastCollectionFailureCode    *string    `json:"last_collection_failure_code,omitempty"`
	LastCollectionFailureMessage *string    `json:"last_collection_failure_message,omitempty"`
	// CollectionIntentID is the live invoice_collection operation, if any.
	CollectionIntentID *uuid.UUID `json:"collection_intent_id,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// InvoiceContact is one billing contact on an invoice or invoice profile.
type InvoiceContact struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// InvoiceLineItem is one statement line on an invoice: a per-event_type usage
// rollup (total amount, event count, summed dimensions).
type InvoiceLineItem struct {
	EventType  string           `json:"event_type"`
	Amount     int64            `json:"amount"`
	Count      int64            `json:"count"`
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
}
