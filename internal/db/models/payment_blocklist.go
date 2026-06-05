package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// PaymentBlocklistEntry is one tenant-scoped block of a known-bad payment
// identifier (issue #300). Checkout/admission can later DENY a payment whose
// (kind, value) matches an entry.
//
// An entry is either tenant-wide (OwnerID nil/zero) — applying to every owner
// org in the tenant — or scoped to a single owner org (OwnerID set, issue #221).
// Uniqueness is (tenant_id, kind, value); that index is also the IsBlocked lookup.
type PaymentBlocklistEntry struct {
	bun.BaseModel `bun:"table:billing.payment_blocklist,alias:pbl"`

	ID uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	// TenantID scopes this row to a tenant / billing namespace (issue #223/#227).
	TenantID uuid.UUID `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id"`
	// OwnerID, when set, scopes the block to a single owner org (issue #221).
	// Nil/zero means a tenant-wide block.
	OwnerID *uuid.UUID `bun:"owner_id,type:uuid,nullzero" json:"owner_id,omitempty"`
	// Kind is the kind of identifier blocked:
	// card_fingerprint | processor_customer | email | ip.
	Kind string `bun:"kind,notnull" json:"kind"`
	// Value is the blocked identifier value.
	Value string `bun:"value,notnull" json:"value"`
	// Reason is an optional free-form note for the block (audit).
	Reason    *string   `bun:"reason,nullzero" json:"reason,omitempty"`
	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
}
