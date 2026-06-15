package models

import (
	"time"

	"github.com/google/uuid"
)

// UsageEvent is one append-only, multi-dimensional record of metered usage
// (issue #289). The host PRICES the event (Amount, in the credit type's smallest
// unit) and OpenRails records it AND debits the ledger in the SAME transaction.
// It is the source of truth for usage reporting + #303 invoice line items.
//
// Idempotency is enforced by a unique index on
// (tenant_id, customer_id, event_type, source, source_id): a replayed metered
// request neither double-records nor double-charges.
type UsageEvent struct {
	ID uuid.UUID `json:"id"`
	// MerchantID scopes this row to a tenant / billing namespace (issue #223/#227).
	MerchantID uuid.UUID `json:"tenant_id"`
	// CustomerID is the tenant subject BILLED for this usage (issue #221, the payer).
	CustomerID uuid.UUID `json:"customer_id"`
	// Actor is the caller-supplied principal string that caused usage
	// (opaque to OpenRails; attribution + grouping only, not the payer).
	Actor string `json:"actor"`
	// InvokerID is the end-user's stable authkit delegated_users id (#491); nil on
	// the embedded/service path (no issuer to resolve).
	InvokerID *uuid.UUID `json:"invoker,omitempty"`
	// Resource is the caller-supplied free-form string for what was metered
	// (opaque to OpenRails; e.g. tensorhub endpoint slug). Nullable.
	Resource *string `json:"resource,omitempty"`
	// EventType is the metered endpoint / model (e.g. "gpt-4o").
	EventType string `json:"event_type"`
	// Dimensions are per-dimension counts (input_tokens, output_tokens,
	// cached_input_tokens, requests, ...). Host-defined.
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
	// Amount is the host-priced cost in the credit type's smallest unit (>= 0).
	Amount int64 `json:"amount"`
	// Source + SourceID form the idempotency key (SourceID is typically the request id).
	Source   string `json:"source"`
	SourceID string `json:"source_id"`
	// MoneyTransactionID links to the ledger debit this event produced.
	MoneyTransactionID *uuid.UUID     `json:"money_transaction_id,omitempty"`
	Metadata           map[string]any `json:"metadata,omitempty"`
	OccurredAt         time.Time      `json:"occurred_at"`
	CreatedAt          time.Time      `json:"created_at"`
}
