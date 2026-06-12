package reconcile

import (
	"time"

	"github.com/google/uuid"
)

// FindingType is the PS-1..PS-9 discrepancy taxonomy from #107.
type FindingType string

const (
	// FindingRemoteSubMissingLocal (PS-1): the processor bills a subscription
	// OpenRails does not know. CRITICAL; always admin_pending in v1 (no
	// auto-create — bootstrap materialization is deferred per design decision 4).
	FindingRemoteSubMissingLocal FindingType = "PS-1"
	// FindingLocalActiveRemoteDead (PS-2): local says active/past_due, the
	// processor says cancelled/expired (on NMI: absent from the recurring
	// report). Enforce: cancel locally + revoke subscription-sourced
	// entitlements.
	FindingLocalActiveRemoteDead FindingType = "PS-2"
	// FindingStatusMismatch (PS-3): statuses/periods disagree in a non-PS-2
	// way. Enforce: adopt the processor's status + period timestamps.
	FindingStatusMismatch FindingType = "PS-3"
	// FindingChargeMissingLocal (PS-4): a successful processor charge has no
	// local payment record. Enforce: backfill billing.payments (+ entitlements
	// when the charge's subscription period is current).
	FindingChargeMissingLocal FindingType = "PS-4"
	// FindingRefundUnrecorded (PS-5): a processor refund is not recorded
	// locally. Enforce: record the refund; any entitlement-revocation
	// recommendation goes to the admin queue.
	FindingRefundUnrecorded FindingType = "PS-5"
	// FindingChargebackActiveSub (PS-6): chargeback at the processor while the
	// matched subscription is still active locally. CRITICAL; admin_pending
	// (terminating a paying-ish user over a dispute is a human decision).
	FindingChargebackActiveSub FindingType = "PS-6"
	// FindingVaultMismatch (PS-7): stored payment-method metadata disagrees
	// with the processor vault. Enforce: adopt the processor record.
	FindingVaultMismatch FindingType = "PS-7"
	// FindingDuplicateSubscriptions (PS-8): one subject carries overlapping
	// active subscriptions. Always admin_pending — the fix (cancel+refund at
	// the processor) is remote and human.
	FindingDuplicateSubscriptions FindingType = "PS-8"
	// FindingEntitlementMismatch (PS-9): entitlements disagree with the
	// subscription, either direction. Enforce: grant/revoke
	// SUBSCRIPTION-sourced entitlements only.
	FindingEntitlementMismatch FindingType = "PS-9"
	// FindingStuckIntent (PS-10): a provider intent (#358 ledger) sat
	// non-terminal beyond the hardcoded stuck thresholds (pending/
	// failed_retryable > 24h; in_flight/unknown_needs_verify > 2h).
	// Provider-independent: emitted on EVERY run from the local ledger alone,
	// regardless of provider filters; no provider calls. Mode/kill-switch
	// parks are informational (the executor drains them when the blocker
	// lifts); everything else is admin_pending — it means provider failures,
	// bad credentials, or a dead worker. Neither check nor fix ever touches
	// the intent itself: the executor/verifier own it.
	FindingStuckIntent FindingType = "PS-10"
)

// Severity of a finding.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
)

// FindingStatus is the persisted finding lifecycle.
type FindingStatus string

const (
	FindingStatusOpen         FindingStatus = "open"
	FindingStatusAutoFixed    FindingStatus = "auto_fixed"
	FindingStatusAdminPending FindingStatus = "admin_pending"
	FindingStatusResolved     FindingStatus = "resolved"
	FindingStatusDismissed    FindingStatus = "dismissed"
)

// Mode selects advisory (diff + report, zero local writes) or enforce
// (one-shot fetch+diff+apply; LOCAL writes only — design decision 2).
type Mode string

const (
	ModeAdvisory Mode = "advisory"
	ModeEnforce  Mode = "enforce"
)

// Finding is one diagnosed discrepancy as emitted by the diff engine, before
// persistence. SubjectKey is the stable identity within (provider, type) —
// design decision 1 — so re-runs update rather than duplicate.
type Finding struct {
	Provider          Provider
	Type              FindingType
	SubjectKey        string
	Severity          Severity
	Status            FindingStatus // open or admin_pending at emit time
	RequiresAdmin     bool
	RecommendedAction string
	LocalEvidence     map[string]any
	RemoteEvidence    map[string]any
	// IntentEvidence carries class-3 local-intent annotation (design decision
	// 5): when local state already records the intent that explains the drift
	// (e.g. DeletionScheduledAt set => the recorded delete never executed),
	// the finding documents it instead of escalating to the admin queue.
	IntentEvidence map[string]any

	// Apply is the enforce instruction derived during the diff; nil when the
	// finding is advisory-only (admin_pending types, intent-annotated drift).
	// Never persisted.
	Apply *ApplyAction `json:"-"`
}

// ApplyAction is one idempotent LOCAL write the enforce mode performs for a
// finding. Exactly one field is set. No ApplyAction ever touches a processor.
type ApplyAction struct {
	CancelLocal        *CancelLocalAction
	AdoptStatus        *AdoptStatusAction
	BackfillPayment    *BackfillPaymentAction
	RecordRefund       *RecordRefundAction
	AdoptVault         *AdoptVaultAction
	GrantEntitlements  *GrantEntitlementsAction
	RevokeEntitlements *RevokeEntitlementsAction
	Materialize        *MaterializeSubscriptionAction
}

// MaterializeSubscriptionAction creates the local subscription for a PS-1
// finding whose identity AND plan both resolved unambiguously (bootstrap mode
// v1.1, the explicit --materialize opt-in). Identity comes from the engine's
// existing matcher (a single vault/email match — zero or multiple candidates
// keep the finding admin_pending), the plan from catalog provider_links (the
// billable price whose processors[provider] ids carry the remote plan id).
// The created subscription snapshots the product's entitlements/credits specs
// like a normal signup, so entitlements flow through the ordinary
// subscription-sourced path.
type MaterializeSubscriptionAction struct {
	Provider Provider
	// Processor is the LOCAL processor name to stamp on the subscription —
	// the key under which the price's provider link matched (e.g. "mobius",
	// "stripe"), so the new row joins the same roster future reconciles load.
	Processor               string
	ProcessorSubscriptionID string
	TenantSubjectID         uuid.UUID
	PriceID                 uuid.UUID
	ProductID               uuid.UUID
	Status                  string // active | past_due (PS-1 only fires for live remote subs)
	PeriodStartsAt          *time.Time
	PeriodEndsAt            *time.Time
	StartedAt               *time.Time
	UserEmail               string
	// IdentityVia documents how identity resolved (vault_id | email) for the
	// resolution evidence.
	IdentityVia string
	// Backfill, when non-nil, records the snapshot's most recent successful
	// charge for this remote subscription after creation. The writer fills in
	// SubscriptionID with the freshly created id.
	Backfill *BackfillPaymentAction
}

// MaterializeResult reports what one materialization actually did.
type MaterializeResult struct {
	SubscriptionID      uuid.UUID
	Created             bool
	EntitlementsGranted int
	PaymentBackfilled   bool
}

// CancelLocalAction cancels a local subscription (PS-2) and revokes its
// subscription-sourced entitlements.
type CancelLocalAction struct {
	SubscriptionID uuid.UUID
	CancelType     string // models.CancelTypeExpired
	Reason         string
}

// AdoptStatusAction adopts the processor's status/periods (PS-3).
type AdoptStatusAction struct {
	SubscriptionID uuid.UUID
	Status         string // billing.subscription_status value
	PeriodStartsAt *time.Time
	PeriodEndsAt   *time.Time
}

// BackfillPaymentAction inserts the missing local payment for a processor
// charge (PS-4), deduped on (tenant, processor, transaction_id), and grants
// the subscription's entitlements when the period is current.
type BackfillPaymentAction struct {
	Processor       string
	TransactionID   string
	AmountCents     int64
	Currency        string
	PurchasedAt     time.Time
	PriceID         uuid.UUID
	SubscriptionID  *uuid.UUID
	TenantSubjectID uuid.UUID
	Metadata        map[string]any
	// Grant, when non-nil, grants entitlements for the current period after
	// the backfill (charge covers a period that is still running).
	Grant *GrantEntitlementsAction
}

// RecordRefundAction records a processor refund locally (PS-5) as a
// negative-amount payment row linked to the refunded payment.
type RecordRefundAction struct {
	Processor         string
	TransactionID     string
	AmountCents       int64 // positive remote amount; recorded negative
	Currency          string
	PurchasedAt       time.Time
	PriceID           uuid.UUID
	SubscriptionID    *uuid.UUID
	RefundedPaymentID *uuid.UUID
	TenantSubjectID   uuid.UUID
	Metadata          map[string]any
	// MarkRefundedOnly skips inserting a refund row and only flips the
	// original payment's status to refunded — used when the refund shares the
	// original transaction id (NMI refund actions ride the original
	// transaction) or no price is resolvable for an insert.
	MarkRefundedOnly bool
}

// AdoptVaultAction adopts processor vault metadata onto a local payment
// method (PS-7).
type AdoptVaultAction struct {
	PaymentMethodID uuid.UUID
	LastFour        string
	ExpiryDate      string
}

// GrantEntitlementsAction grants subscription-sourced entitlement windows
// (PS-9 grant direction / PS-4 current-period grant).
type GrantEntitlementsAction struct {
	SubscriptionID  uuid.UUID
	TenantSubjectID uuid.UUID
	Entitlements    []string
	StartAt         time.Time
	EndAt           *time.Time
}

// RevokeEntitlementsAction revokes the LIVE subscription-sourced entitlements
// of one subscription (PS-9 revoke direction). Admin grants and grace windows
// are different source types and are never touched.
type RevokeEntitlementsAction struct {
	SubscriptionID uuid.UUID
	Reason         string
}

// stateRosterFindingTypes are the finding types whose subjects are fully
// re-enumerated by every run against the provider's CURRENT state roster, so
// absence from a completed run means the discrepancy vanished.
// Transaction-window types (PS-4/5/6) only auto-resolve when the run's window
// re-covered the transaction (handled per-finding by the engine).
var stateRosterFindingTypes = []FindingType{
	FindingRemoteSubMissingLocal,
	FindingLocalActiveRemoteDead,
	FindingStatusMismatch,
	FindingVaultMismatch,
	FindingDuplicateSubscriptions,
	FindingEntitlementMismatch,
}

func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 0
	case SeverityHigh:
		return 1
	case SeverityMedium:
		return 2
	default:
		return 3
	}
}
