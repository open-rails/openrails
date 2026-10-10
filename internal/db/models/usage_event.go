package models

import (
	"time"

	"github.com/google/uuid"
)

// UsageEvent is one append-only record of metered usage, the source of truth
// for usage reporting and invoice line items. Host-priced events carry final
// cost; catalog events carry an unpriced meter input. Host cost is debited in
// the same transaction. A replay is found by (merchant, customer, currency,
// event_type, source, source_id) within the ingest window, so it neither
// double-records nor double-charges.
type UsageEvent struct {
	ID uuid.UUID `json:"id"`
	// MerchantID scopes this row to a merchant.
	MerchantID uuid.UUID `json:"merchant_id"`
	// CustomerID is the customer billed for this usage.
	CustomerID uuid.UUID `json:"customer_id"`
	// Invoker is the caller-supplied principal string that caused usage
	// (opaque to OpenRails; attribution + grouping only, not the payer).
	Invoker string `json:"invoker"`
	// Currency is the native OpenRails currency this usage amount is denominated in.
	Currency string `json:"currency"`
	// Resource is the caller-supplied free-form string for what was metered
	// (opaque to OpenRails; for example, an endpoint slug). Nullable.
	Resource *string `json:"resource,omitempty"`
	// EventType is the metered endpoint / model (e.g. "gpt-4o").
	EventType string `json:"event_type"`
	// Dimensions are per-dimension counts (input_tokens, output_tokens,
	// cached_input_tokens, requests, ...). Host-defined.
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
	// Amount is the cost or meter input in Currency's internal precision (>= 0).
	// PricingAuthority determines whether catalog rating may consume this event.
	Amount int64 `json:"amount"`
	// Outcome is succeeded or failed; ForgivenAmount is what a failed event's
	// grace absorbed, so Amount + ForgivenAmount is what the host reported.
	Outcome        string `json:"outcome"`
	ForgivenAmount int64  `json:"forgiven_amount"`
	// Source + SourceID form the idempotency key (SourceID is typically the request id).
	Source   string `json:"source"`
	SourceID string `json:"source_id"`
	// LedgerTransferID links to the ledger debit transfer this event produced.
	LedgerTransferID *uuid.UUID `json:"ledger_transfer_id,omitempty"`
	// PricingAuthority identifies whether amount is final host pricing or catalog input.
	PricingAuthority string         `json:"-"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	OccurredAt       time.Time      `json:"occurred_at"`
	CreatedAt        time.Time      `json:"created_at"`
	// Replayed reports that this event's idempotency coordinate was already
	// recorded, so this call metered nothing and moved no money. Not
	// persisted.
	Replayed bool `json:"replayed,omitempty"`
}
