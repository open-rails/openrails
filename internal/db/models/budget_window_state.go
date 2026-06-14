package models

import (
	"time"

	"github.com/google/uuid"
)

// BudgetWindowState is the per-(tenant, tenant subject, actor, window_key)
// fixed-window anchor row (#337, migration 005).
//
// Budget windows are FIXED with knowable reset boundaries, anchored to each
// user's own first charged request (so boundaries are per-user staggered and
// there is no global reset moment):
//
//   - Cadence "session": the window opens at the first charged request when no
//     window is active and closes exactly WindowSeconds later; the next window
//     opens on the next charged request. WindowStart is rewritten on reopen.
//   - Cadence "fixed": boundaries tick at Anchor + k*WindowSeconds forever
//     (same wall-clock reset each period). WindowStart records the most
//     recently opened window; the CURRENT start is derived from Anchor at read
//     time and the row is never rewritten to advance.
//
// Reserve locks this row FOR UPDATE so concurrent reserves around a boundary
// serialize on it.
type BudgetWindowState struct {
	ID uuid.UUID `json:"id"`

	MerchantID uuid.UUID `json:"tenant_id"`

	CustomerID uuid.UUID `json:"customer_id"`

	Actor string `json:"actor"`

	// WindowKey matches BudgetWindow.Key from the caller (e.g. "5h", "7d").
	WindowKey string `json:"window_key"`

	// Cadence is "session" or "fixed" (see type comment).
	Cadence string `json:"cadence"`

	WindowSeconds int64 `json:"window_seconds"`

	// Anchor is the first-ever window open for this key; fixed-cadence
	// boundaries derive from it.
	Anchor time.Time `json:"anchor"`

	// WindowStart is the start of the most recently OPENED window
	// (authoritative for session cadence).
	WindowStart time.Time `json:"window_start"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
