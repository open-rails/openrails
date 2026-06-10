package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
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
	bun.BaseModel `bun:"table:billing.budget_window_state,alias:bws"`

	ID uuid.UUID `bun:"id,pk,type:uuid" json:"id"`

	TenantID uuid.UUID `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id"`

	TenantSubjectID uuid.UUID `bun:"tenant_subject_id,type:uuid,nullzero" json:"tenant_subject_id"`

	Actor string `bun:"actor,notnull" json:"actor"`

	// WindowKey matches BudgetWindow.Key from the caller (e.g. "5h", "7d").
	WindowKey string `bun:"window_key,notnull" json:"window_key"`

	// Cadence is "session" or "fixed" (see type comment).
	Cadence string `bun:"cadence,notnull" json:"cadence"`

	WindowSeconds int64 `bun:"window_seconds,notnull" json:"window_seconds"`

	// Anchor is the first-ever window open for this key; fixed-cadence
	// boundaries derive from it.
	Anchor time.Time `bun:"anchor,notnull" json:"anchor"`

	// WindowStart is the start of the most recently OPENED window
	// (authoritative for session cadence).
	WindowStart time.Time `bun:"window_start,notnull" json:"window_start"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}
