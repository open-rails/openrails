package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// UsageEvent is one append-only, multi-dimensional record of metered usage
// (issue #289). The host PRICES the event (Amount, in the credit type's smallest
// unit) and OpenRails records it AND debits the ledger in the SAME transaction.
// It is the source of truth for usage reporting + #303 invoice line items.
//
// Idempotency is enforced by a unique index on
// (tenant_id, tenant_subject_id, event_type, source, source_id): a replayed metered
// request neither double-records nor double-charges.
type UsageEvent struct {
	bun.BaseModel `bun:"table:billing.usage_events,alias:ue"`

	ID uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	// TenantID scopes this row to a tenant / billing namespace (issue #223/#227).
	TenantID uuid.UUID `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id"`
	// TenantSubjectID is the tenant subject BILLED for this usage (issue #221, the payer).
	TenantSubjectID uuid.UUID `bun:"tenant_subject_id,type:uuid,nullzero" json:"tenant_subject_id"`
	// InvokerID is the invoker that caused usage (attribution only; not the payer).
	InvokerID string `bun:"invoker_id,notnull" json:"invoker_id"`
	// CreditTypeID is the credit type debited for this event.
	CreditTypeID uuid.UUID `bun:"credit_type_id,type:uuid,notnull" json:"credit_type_id"`
	// EventType is the metered endpoint / model (e.g. "gpt-4o").
	EventType string `bun:"event_type,notnull" json:"event_type"`
	// Dimensions are per-dimension counts (input_tokens, output_tokens,
	// cached_input_tokens, requests, ...). Host-defined.
	Dimensions map[string]int64 `bun:"dimensions,type:jsonb,nullzero" json:"dimensions,omitempty"`
	// Amount is the host-priced cost in the credit type's smallest unit (>= 0).
	Amount int64 `bun:"amount,notnull" json:"amount"`
	// Source + SourceID form the idempotency key (SourceID is typically the request id).
	Source   string `bun:"source,notnull" json:"source"`
	SourceID string `bun:"source_id,notnull" json:"source_id"`
	// CreditTransactionID links to the ledger debit this event produced.
	CreditTransactionID *uuid.UUID     `bun:"credit_transaction_id,type:uuid,nullzero" json:"credit_transaction_id,omitempty"`
	Metadata            map[string]any `bun:"metadata,type:jsonb,nullzero" json:"metadata,omitempty"`
	OccurredAt          time.Time      `bun:"occurred_at,notnull" json:"occurred_at"`
	CreatedAt           time.Time      `bun:"created_at,notnull" json:"created_at"`
}
