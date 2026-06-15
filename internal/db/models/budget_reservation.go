package models

import (
	"time"

	"github.com/google/uuid"
)

// BudgetReservation is one in-flight or settled charge against a delegated
// invoker's rolling money-budget windows (issue #304). The budget engine
// (internal/modules/budgets) computes per-window used/reserved/remaining as
// windowed SUM() over these rows by CreatedAt — the windows themselves are
// PASSED IN by the caller, never read from any tier table.
//
// Lifecycle: Reserve -> "active" (counts against `reserved` by Amount);
// Capture -> "captured" (counts against `used` by Captured);
// Release -> "released" (counts against neither).
//
// Idempotency is enforced by a unique index on
// (tenant_id, customer_id, invoker, currency, source, source_id): a replayed
// Reserve returns the existing row rather than double-reserving.
type BudgetReservation struct {
	ID uuid.UUID `json:"id"`
	// MerchantID scopes this row to a tenant / billing namespace (issue #223/#227).
	MerchantID uuid.UUID `json:"tenant_id"`
	// CustomerID is the tenant subject the budget is charged against (issue #221, the payer).
	CustomerID uuid.UUID `json:"customer_id"`
	// Invoker is the caller-supplied principal string whose spend the windows cap.
	Invoker string `json:"invoker"`
	// Currency is the native OpenRails currency this hold/window is denominated in.
	Currency string `json:"currency"`
	// Amount is the reserved amount in the row currency's internal precision;
	// it counts against `reserved` while Status == "active".
	Amount int64 `json:"amount"`
	// Captured is the actually captured amount; counts against `used` once
	// Status == "captured". 0 until captured.
	Captured int64 `json:"captured"`
	// Status is one of "active", "captured", "released".
	Status string `json:"status"`
	// Source + SourceID form the idempotency key (SourceID is typically the request id).
	Source    string     `json:"source"`
	SourceID  string     `json:"source_id"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}
