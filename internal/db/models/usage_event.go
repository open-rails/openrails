package models

import (
	"time"

	"github.com/open-rails/openrails/catalog"

	"github.com/google/uuid"
)

// UsageEvent is one append-only record of usage and its posted meter charges.
// A replay is found by (merchant, customer, currency, event_type, source,
// source_id) within the ingest window, so it neither records nor charges twice.
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
	// Amount is what the event charged in Currency's internal precision (>= 0).
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
	// StatedAmount is the optional cost supplied by the host, independent of
	// what the meters charged. Failed events always state their cost.
	StatedAmount *int64 `json:"stated_amount,string"`
	// Charges are what each priced meter charged the event when it was
	// recorded; Amount is their sum.
	Charges    []UsageCharge  `json:"charges"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	OccurredAt time.Time      `json:"occurred_at"`
	CreatedAt  time.Time      `json:"created_at"`
	// Replayed reports that this event's idempotency coordinate was already
	// recorded, so this call metered nothing and moved no money. Not
	// persisted.
	Replayed bool `json:"replayed,omitempty"`
}

// UsageCharge is what one meter charged one usage event: its quantity in the
// meter's unit, in the matrix cell it was priced in (empty without a matrix).
type UsageCharge struct {
	MeterKey string        `json:"meter_key"`
	Model    catalog.Model `json:"model"`
	Cell     string        `json:"cell,omitempty"`
	Quantity int64         `json:"quantity"`
	Amount   int64         `json:"amount,string"`
}
