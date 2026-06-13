package models

import (
	"time"

	"github.com/google/uuid"
)

// Invoice is a monthly itemized statement for a customer (issue #303): the
// human-readable record of what was billed in a period, for prepaid and arrears
// alike. Line items + money movements are rolled up from openrails.usage_events
// and the credit ledger, snapshotted at finalize for immutability.
type Invoice struct {
	ID              uuid.UUID `json:"id"`
	TenantID        uuid.UUID `json:"tenant_id"`
	TenantSubjectID uuid.UUID `json:"tenant_subject_id"`
	CreditTypeID    uuid.UUID `json:"credit_type_id"`
	Currency        string    `json:"currency"`

	PeriodFrom time.Time `json:"period_from"`
	PeriodTo   time.Time `json:"period_to"`

	UsageTotal     int64 `json:"usage_total"`
	DepositsTotal  int64 `json:"deposits_total"`
	OwedAccrued    int64 `json:"owed_accrued"`
	OwedPaid       int64 `json:"owed_paid"`
	ClosingBalance int64 `json:"closing_balance"`

	// LineItems is the per-event_type usage rollup snapshot (amount, count,
	// dimensions). MoneyMovements is the summed-by-type ledger snapshot.
	LineItems      []InvoiceLineItem `json:"line_items"`
	MoneyMovements map[string]int64  `json:"money_movements"`

	Status      string     `json:"status"`
	FinalizedAt *time.Time `json:"finalized_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// InvoiceLineItem is one metered-usage line on an invoice: the per-event_type
// (per model/endpoint) total amount, event count, and summed dimensions.
type InvoiceLineItem struct {
	EventType  string           `json:"event_type"`
	Amount     int64            `json:"amount"`
	Count      int64            `json:"count"`
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
}
