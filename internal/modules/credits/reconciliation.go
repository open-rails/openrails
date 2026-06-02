package credits

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ExpectedSettlement is one cross-system reconciliation input (issue #243): a
// settlement the HOST expects to find in the OpenRails ledger because a
// Tensorhub usage/billing event was emitted for it. The reconciliation job
// diffs a batch of these against billing.credit_transactions to find:
//
//   - held-but-never-settled requests / lost settle calls — an expected
//     settlement with NO matching capture/withdrawal in the ledger; and
//   - double-charge candidates — a ledger capture/withdrawal with NO expected
//     settlement feeding it.
//
// Tensorhub data is NOT in this repo, so OpenRails owns only the DIFF interface
// and report surface: the host (gen-orchestrator / processor-sync, see #107)
// collects the day's endpoint_billing_events and supplies them as
// []ExpectedSettlement. The matching key is the request idempotency key, which
// OpenRails stores as credit_transactions.source_id.
type ExpectedSettlement struct {
	// TenantID / OwnerID scope the expectation (issue #221/#223). UserID is the
	// actor for attribution only.
	TenantID     uuid.UUID
	OwnerID      uuid.UUID
	UserID       string
	CreditTypeID uuid.UUID
	// Source is the credit_transactions.source the settlement is expected under
	// (e.g. "usage", "api_call"). Empty matches any source.
	Source string
	// SourceID is the request idempotency key — credit_transactions.source_id.
	// This is the join key against the OpenRails ledger.
	SourceID string
	// Amount is the expected settled amount (absolute value of the captured
	// spend), for value-level drift reporting. Zero means amount is unknown / not
	// checked.
	Amount int64
	// EmittedAt is when Tensorhub emitted the underlying usage/billing event.
	EmittedAt time.Time
}

// ReconciliationEventKind classifies a SettlementReconciliationEvent. It mirrors
// the persisted models.ReconciliationKind values so a sink can route on them
// without importing internal/db/models.
type ReconciliationEventKind string

const (
	// ReconMissingSettlement: an expected settlement has no matching ledger
	// capture/withdrawal (held-but-never-settled / lost settle).
	ReconMissingSettlement ReconciliationEventKind = "missing_settlement"
	// ReconUnexpectedCapture: a ledger capture/withdrawal has no expected
	// settlement feeding it (double-charge candidate).
	ReconUnexpectedCapture ReconciliationEventKind = "unexpected_capture"
	// ReconOrphanHold: an active hold past its expiry that was (or will be)
	// released by the cleanup pass.
	ReconOrphanHold ReconciliationEventKind = "orphan_hold"
	// ReconHeldBalanceDrift: denormalized held_balance disagreed with the sum of
	// active holds.
	ReconHeldBalanceDrift ReconciliationEventKind = "held_balance_drift"
	// ReconBalanceDrift: denormalized balance disagreed with the ledger.
	ReconBalanceDrift ReconciliationEventKind = "balance_drift"
)

// ReconciliationEvent is the reusable reconciliation SIGNAL emitted when the
// reconciliation loop detects a divergence (issue #243). It is the alert-first
// hook: the job persists every divergence to billing.reconciliation_events AND
// emits this signal to a sink, so downstream consumers (ops paging, dashboards,
// or a future auto-remediation policy) can react without re-deriving it.
//
// The signal is owner/tenant-scoped; OwnerID/TenantID/CreditTypeID may be nil
// for cross-system orphans that carry no owner context.
type ReconciliationEvent struct {
	Kind         ReconciliationEventKind
	TenantID     *uuid.UUID
	OwnerID      *uuid.UUID
	CreditTypeID *uuid.UUID
	UserID       string
	// SubjectID identifies the concrete subject (hold id, balance row id,
	// settlement source_id, capture id).
	SubjectID string
	// Expected / Observed carry the diverging values for drift/diff kinds.
	Expected int64
	Observed int64
	// Remediated is true when the loop safely auto-repaired the divergence
	// (orphan hold released, held_balance corrected). Alert-only kinds are false.
	Remediated bool
	DetectedAt time.Time
}

// ReconciliationSink consumes ReconciliationEvents. Like LowBalanceSink (#240),
// the reconciliation job emits to a sink rather than a concrete notifier so the
// SAME detection loop can fan the alert-first signal out to several consumers
// (ops notification, metrics, a future auto-remediation policy) by composing
// sinks — without the job knowing about any of them.
//
// Handle should be best-effort: a sink error is logged by the job but never
// rolls back a persisted reconciliation_events row or a completed remediation.
type ReconciliationSink interface {
	Handle(ctx context.Context, ev ReconciliationEvent) error
}

// ReconciliationSinkFunc adapts a plain function to ReconciliationSink.
type ReconciliationSinkFunc func(ctx context.Context, ev ReconciliationEvent) error

func (f ReconciliationSinkFunc) Handle(ctx context.Context, ev ReconciliationEvent) error {
	return f(ctx, ev)
}

// MultiReconciliationSink fans a ReconciliationEvent out to several sinks in
// order, collecting the first non-nil error after every sink has been given the
// event. It is the composition point for additional consumers.
type MultiReconciliationSink []ReconciliationSink

func (m MultiReconciliationSink) Handle(ctx context.Context, ev ReconciliationEvent) error {
	var firstErr error
	for _, s := range m {
		if s == nil {
			continue
		}
		if err := s.Handle(ctx, ev); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
