package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// ReconciliationKind classifies a row in billing.reconciliation_events. Each
// kind is the alert-first record of one divergence the billing reconciliation
// loop (issue #243) detected, written BEFORE any safe auto-remediation.
//
//   - ReconciliationOrphanHold: a transaction_type='hold' row stuck in
//     status='active' past its expires_at (a worker died without capture/
//     release). Remediated: released (status->'expired', held_balance restored).
//   - ReconciliationHeldBalanceDrift: per (tenant, owner, credit_type) the
//     denormalized user_credit_balances.held_balance disagrees with the sum of
//     authorized_amount over still-active holds. Remediated: held_balance
//     corrected to the ledger-derived value.
//   - ReconciliationBalanceDrift: per (tenant, owner, credit_type) the
//     denormalized user_credit_balances.balance disagrees with the
//     credit_transactions ledger. ALERT-ONLY (reported, never auto-corrected).
//   - ReconciliationMissingSettlement: a cross-system diff — an expected
//     settlement (fed by the host from a Tensorhub usage/billing event) has no
//     matching capture/withdrawal in the OpenRails ledger (held-but-never-
//     settled or a lost settle call). ALERT-ONLY.
//   - ReconciliationUnexpectedCapture: the symmetric cross-system diff — an
//     OpenRails capture/withdrawal has no expected settlement feeding it
//     (double-charge candidate). ALERT-ONLY.
type ReconciliationKind string

const (
	ReconciliationOrphanHold        ReconciliationKind = "orphan_hold"
	ReconciliationHeldBalanceDrift  ReconciliationKind = "held_balance_drift"
	ReconciliationBalanceDrift      ReconciliationKind = "balance_drift"
	ReconciliationMissingSettlement ReconciliationKind = "missing_settlement"
	ReconciliationUnexpectedCapture ReconciliationKind = "unexpected_capture"
)

// ReconciliationEvent is an alert-first record produced by the billing
// reconciliation + orphan-hold cleanup loop (issue #243). It mirrors the
// alert-only CatalogDriftEvent (issue #209): the loop records every divergence
// here independently of any safe auto-remediation, so operators always see what
// was detected even when nothing was repaired. An event is "open" while
// ResolvedAt IS NULL. Rows dedupe on (tenant, owner, credit_type, kind,
// subject_id) so reruns are idempotent.
//
// Owner/tenant-scoped (issue #221/#223): TenantID + OwnerID are the billing
// namespace + billing owner. They are nil only for cross-system orphans that
// carry no owner context; the OpenRails-internal checks always set them.
type ReconciliationEvent struct {
	bun.BaseModel `bun:"table:billing.reconciliation_events,alias:rce"`

	ID uuid.UUID `bun:"id,pk,type:uuid,default:uuidv7()" json:"id"`

	TenantID     *uuid.UUID `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id,omitempty"`
	OwnerID      *uuid.UUID `bun:"owner_id,type:uuid,nullzero" json:"owner_id,omitempty"`
	CreditTypeID *uuid.UUID `bun:"credit_type_id,type:uuid,nullzero" json:"credit_type_id,omitempty"`

	Kind ReconciliationKind `bun:"kind,notnull" json:"kind"`

	// SubjectID stably identifies the subject so open rows dedupe: the hold
	// transaction id for orphan_hold, the balance row id for *_drift, the
	// expected-settlement key for missing_settlement, the capture transaction id
	// for unexpected_capture.
	SubjectID string `bun:"subject_id,nullzero" json:"subject_id,omitempty"`

	// ExpectedValue / ObservedValue carry the stringified diverging values for
	// drift/diff kinds (e.g. ledger-derived sum vs. denormalized held_balance).
	ExpectedValue string `bun:"expected_value,nullzero" json:"expected_value,omitempty"`
	ObservedValue string `bun:"observed_value,nullzero" json:"observed_value,omitempty"`

	// RemediatedAt is stamped when the loop SAFELY auto-repaired the divergence
	// (released an orphan hold, corrected held_balance). NULL = alert-only or
	// not-yet-remediated.
	RemediatedAt *time.Time `bun:"remediated_at,nullzero" json:"remediated_at,omitempty"`

	DetectedAt time.Time  `bun:"detected_at,notnull,default:current_timestamp" json:"detected_at"`
	ResolvedAt *time.Time `bun:"resolved_at,nullzero" json:"resolved_at,omitempty"`
}
