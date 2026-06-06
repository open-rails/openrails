package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// Invoice is a monthly itemized statement for a customer (issue #303): the
// human-readable record of what was billed in a period, for prepaid and arrears
// alike. Line items + money movements are rolled up from billing.usage_events
// and the credit ledger, snapshotted at finalize for immutability.
type Invoice struct {
	bun.BaseModel `bun:"table:billing.invoices,alias:inv"`

	ID              uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	TenantID        uuid.UUID `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id"`
	TenantSubjectID uuid.UUID `bun:"tenant_subject_id,type:uuid,nullzero" json:"tenant_subject_id"`
	CreditTypeID    uuid.UUID `bun:"credit_type_id,type:uuid,notnull" json:"credit_type_id"`
	Currency        string    `bun:"currency,notnull" json:"currency"`

	PeriodFrom time.Time `bun:"period_from,notnull" json:"period_from"`
	PeriodTo   time.Time `bun:"period_to,notnull" json:"period_to"`

	UsageTotal     int64 `bun:"usage_total,notnull" json:"usage_total"`
	DepositsTotal  int64 `bun:"deposits_total,notnull" json:"deposits_total"`
	OwedAccrued    int64 `bun:"owed_accrued,notnull" json:"owed_accrued"`
	OwedPaid       int64 `bun:"owed_paid,notnull" json:"owed_paid"`
	ClosingBalance int64 `bun:"closing_balance,notnull" json:"closing_balance"`

	// LineItems is the per-event_type usage rollup snapshot (amount, count,
	// dimensions). MoneyMovements is the summed-by-type ledger snapshot.
	LineItems      []InvoiceLineItem `bun:"line_items,type:jsonb,nullzero" json:"line_items"`
	MoneyMovements map[string]int64  `bun:"money_movements,type:jsonb,nullzero" json:"money_movements"`

	Status      string     `bun:"status,notnull" json:"status"`
	FinalizedAt *time.Time `bun:"finalized_at,nullzero" json:"finalized_at,omitempty"`
	CreatedAt   time.Time  `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt   time.Time  `bun:"updated_at,notnull" json:"updated_at"`
}

// InvoiceLineItem is one metered-usage line on an invoice: the per-event_type
// (per model/endpoint) total amount, event count, and summed dimensions.
type InvoiceLineItem struct {
	EventType  string           `json:"event_type"`
	Amount     int64            `json:"amount"`
	Count      int64            `json:"count"`
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
}
