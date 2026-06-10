package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// BudgetReservation is one in-flight or settled charge against a delegated
// actor's rolling money-budget windows (issue #304). The budget engine
// (internal/modules/budgets) computes per-window used/reserved/remaining as
// windowed SUM() over these rows by CreatedAt — the windows themselves are
// PASSED IN by the caller, never read from any tier table.
//
// Lifecycle: Reserve -> "active" (counts against `reserved` by AmountMillicents);
// Capture -> "captured" (counts against `used` by CapturedMillicents);
// Release -> "released" (counts against neither).
//
// Idempotency is enforced by a unique index on
// (tenant_id, tenant_subject_id, actor, source, source_id): a replayed Reserve returns
// the existing row rather than double-reserving.
type BudgetReservation struct {
	bun.BaseModel `bun:"table:billing.budget_reservations,alias:br"`

	ID uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	// TenantID scopes this row to a tenant / billing namespace (issue #223/#227).
	TenantID uuid.UUID `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id"`
	// TenantSubjectID is the tenant subject the budget is charged against (issue #221, the payer).
	TenantSubjectID uuid.UUID `bun:"tenant_subject_id,type:uuid,nullzero" json:"tenant_subject_id"`
	// Actor is the caller-supplied principal string whose spend the windows cap.
	Actor string `bun:"actor,notnull" json:"actor"`
	// AmountMillicents is the reserved (authorized) amount; counts against
	// `reserved` while Status == "active".
	AmountMillicents int64 `bun:"amount_millicents,notnull" json:"amount_millicents"`
	// CapturedMillicents is the actually captured amount; counts against `used`
	// once Status == "captured". 0 until captured.
	CapturedMillicents int64 `bun:"captured_millicents,notnull" json:"captured_millicents"`
	// Status is one of "active", "captured", "released".
	Status string `bun:"status,notnull" json:"status"`
	// Source + SourceID form the idempotency key (SourceID is typically the request id).
	Source    string     `bun:"source,notnull" json:"source"`
	SourceID  string     `bun:"source_id,notnull" json:"source_id"`
	CreatedAt time.Time  `bun:"created_at,notnull" json:"created_at"`
	ExpiresAt *time.Time `bun:"expires_at,nullzero" json:"expires_at,omitempty"`
}
