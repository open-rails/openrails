package reconcile

import (
	"time"

	"github.com/google/uuid"
)

// FindingType is the PS-1..PS-9 discrepancy taxonomy from #107.
type FindingType string

const (
	// FindingRemoteSubMissingLocal (PS-1): the rail bills a subscription
	// OpenRails does not know. CRITICAL. Enforce materializes the local
	// subscription when identity AND plan resolve unambiguously; ambiguous or
	// unresolvable findings stay requires_review.
	FindingRemoteSubMissingLocal FindingType = "pull.subscription.missing"
	// FindingLocalActiveRemoteDead (PS-2): local says active/past_due, the
	// rail says cancelled/expired (on NMI: absent from the recurring
	// report). Enforce: cancel locally + revoke subscription-sourced
	// entitlements.
	FindingLocalActiveRemoteDead FindingType = "pull.subscription.dead"
	// FindingStatusMismatch (PS-3): statuses/periods disagree in a non-PS-2
	// way. Enforce: adopt the rail's status + period timestamps.
	FindingStatusMismatch FindingType = "pull.subscription.mismatch"
	// FindingChargeMissingLocal (PS-4): a successful rail charge has no
	// local payment record. Enforce: backfill openrails.payments (+ entitlements
	// when the charge's subscription period is current).
	FindingChargeMissingLocal FindingType = "pull.charge.missing"
	// FindingRefundUnrecorded (PS-5): a rail refund is not recorded
	// locally. Enforce: record the refund; any entitlement-revocation
	// recommendation goes to the admin queue.
	FindingRefundUnrecorded FindingType = "pull.refund.missing"
	// FindingChargebackActiveSub (PS-6): chargeback at the rail while the
	// matched subscription is still active locally. CRITICAL; requires_review
	// (terminating a paying-ish user over a dispute is a human decision).
	FindingChargebackActiveSub FindingType = "pull.dispute.chargeback"
	// FindingPaymentMethodMismatch (PS-7): stored payment-method metadata disagrees
	// with the rail vault. Enforce: adopt the rail record.
	FindingPaymentMethodMismatch FindingType = "pull.payment_method.mismatch"
	// FindingDuplicateSubscriptions (PS-8): one subject carries overlapping
	// live REMOTE subscriptions. Only the provider snapshot can see this
	// (local duplicates are schema-blocked), so it is a PULL-plane finding
	// (#665 single-writer rule; renamed from consistency.duplicate.subscription
	// by migration 058). Always requires_review — the fix (cancel+refund at
	// the rail) is remote and human.
	FindingDuplicateSubscriptions FindingType = "pull.subscription.duplicate"
	// FindingEvidenceStale (#835): a terminal cancel was WITHHELD because the
	// evidence justifying it predates this deployment's first pull of the
	// merchant (or carries no date at all) — inherited history that was never
	// corroborated by anything we observed. The row parks as `unknown` with its
	// access intact. Always requires_review: only an operator can say whether
	// an imported record is true, and a withheld action must be visible rather
	// than a silent no-op ("unchecked ≠ disappeared").
	//
	// Deliberately NOT in stateRosterFindingTypes: the unknown-cohort and
	// webhook-converge planes write it too, so auto-resolving it on absence
	// from a pull run would erase another plane's open finding.
	FindingEvidenceStale FindingType = "pull.subscription.evidence_stale"
	// FindingCancellationCapped (#837): one pass planned more cancellations
	// than the merchant's per-pass budget allows, so NONE were applied and the
	// pass halted. Always requires_review — a book-sized cancellation is a
	// human decision, never an automatic one.
	FindingCancellationCapped FindingType = "pull.cancellation.capped"
)

// #665 single-writer-per-invariant: the legacy PS-9 entitlement check
// (derive.grant_effect.mismatch) moved into the Convergence Engine's DERIVE
// pass and PS-10 (life.provider_intent.stuck) into its LIFE pass — see
// internal/reconcile/converge/converge_passes.go. The pull engine emits
// pull.* findings only.

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
	FindingStatusAutoFixed         FindingStatus = "auto_fixed"
	FindingStatusReconcileRequired FindingStatus = "reconcile_required"
	FindingStatusRequiresReview    FindingStatus = "requires_review"
	FindingStatusAdminRequired     FindingStatus = FindingStatusRequiresReview
	FindingStatusFixed             FindingStatus = "fixed"
	FindingStatusIgnored           FindingStatus = "ignored"
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
	Status            FindingStatus // reconcile_required or requires_review at emit time
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
	// finding is advisory-only (requires_review types, intent-annotated drift).
	// Never persisted.
	Apply *ApplyAction `json:"-"`
}

// ApplyAction is one idempotent LOCAL write the enforce mode performs for a
// finding. Exactly one field is set. No ApplyAction ever touches a rail.
// Mirror writes (payments / refunds / vault metadata / subscription
// materialization) are direct appliers; subscription STATE transitions are a
// Decide action — the #665 decider is the only thing that moves lifecycle state.
type ApplyAction struct {
	Decide             *DecideAction
	BackfillPayment    *BackfillPaymentAction
	RecordRefund       *RecordRefundAction
	AdoptPaymentMethod *AdoptPaymentMethodAction
	Materialize        *MaterializeSubscriptionAction
}

// DecideAction carries a decider transition computed at diff time from the
// snapshot evidence (#665 mirror-writer refactor: the pull engine invokes the
// decider instead of writing domain state). Applied through the engine's
// DecisionApplier under the same mutation-policy gate the legacy appliers had.
type DecideAction struct {
	SubscriptionID uuid.UUID
	Decision       Decision
}

// MaterializeSubscriptionAction creates the local subscription for a PS-1
// finding whose identity AND plan both resolved unambiguously (applied
// automatically in enforce mode). Identity comes from the engine's
// existing matcher (a single vault/email match — zero or multiple candidates
// keep the finding requires_review), the plan from catalog provider_links (the
// billable price whose rails[provider] ids carry the remote plan id).
// The created subscription snapshots the product's entitlements/credits specs
// like a normal signup, so entitlements flow through the ordinary
// subscription-sourced path.
type MaterializeSubscriptionAction struct {
	Provider Provider
	// PspID is openrails.psps.id for account-bound
	// provider-pull materialization.
	PspID *uuid.UUID
	// Rail is the LOCAL rail name to stamp on the subscription —
	// the key under which the price's provider link matched (e.g. "mobius",
	// "stripe"), so the new row joins the same roster future reconciles load.
	Rail               string
	RailSubscriptionID string
	CustomerID         uuid.UUID
	PriceID            uuid.UUID
	ProductID          uuid.UUID
	Status             string // active | past_due (PS-1 only fires for live remote subs)
	PeriodStartsAt     *time.Time
	PeriodEndsAt       *time.Time
	StartedAt          *time.Time
	UserEmail          string
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

// BackfillPaymentAction inserts the missing local payment for a rail
// charge (PS-4), deduped on (tenant, rail, transaction_id), and grants
// the subscription's entitlements when the period is current.
type BackfillPaymentAction struct {
	PspID          *uuid.UUID
	Rail           string
	TransactionID  string
	AmountCents    int64
	Currency       string
	PurchasedAt    time.Time
	PriceID        uuid.UUID
	SubscriptionID *uuid.UUID
	CustomerID     uuid.UUID
	Metadata       map[string]any
	// Grant, when non-nil, grants entitlements for the current period after
	// the backfill (charge covers a period that is still running).
	Grant *GrantEntitlementsAction
}

// RecordRefundAction records a rail refund locally (PS-5) as a
// negative-amount payment row linked to the refunded payment.
type RecordRefundAction struct {
	PspID             *uuid.UUID
	Rail              string
	TransactionID     string
	AmountCents       int64 // positive remote amount; recorded negative
	Currency          string
	PurchasedAt       time.Time
	PriceID           uuid.UUID
	SubscriptionID    *uuid.UUID
	RefundedPaymentID *uuid.UUID
	CustomerID        uuid.UUID
	Metadata          map[string]any
	// MarkRefundedOnly skips inserting a refund row and only flips the
	// original payment's status to refunded — used when the refund shares the
	// original transaction id (NMI refund actions ride the original
	// transaction) or no price is resolvable for an insert.
	MarkRefundedOnly bool
}

// AdoptPaymentMethodAction adopts rail vault metadata onto a local payment
// method (PS-7).
type AdoptPaymentMethodAction struct {
	PaymentMethodID uuid.UUID
	LastFour        string
	ExpiryDate      string
}

// GrantEntitlementsAction grants subscription-sourced entitlement windows
// (PS-4 current-period grant / PS-1 materialization).
type GrantEntitlementsAction struct {
	SubscriptionID uuid.UUID
	CustomerID     uuid.UUID
	Entitlements   []string
	StartAt        time.Time
	EndAt          *time.Time
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
	FindingPaymentMethodMismatch,
	FindingDuplicateSubscriptions,
}

// SeverityRank orders severities worst-first (critical=0 .. low=3) for sorting
// and escalation comparisons (a #787 FindingNotifier re-fires only when the
// rank strictly decreases — a genuine escalation).
func SeverityRank(s Severity) int {
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
