package reconcile

import (
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
)

// FindingType is the PS-1..PS-9 pull discrepancy taxonomy.
type FindingType string

const (
	// FindingRemoteSubMissingLocal (PS-1): the rail bills a subscription
	// OpenRails does not know. CRITICAL. Enforce materializes the local
	// subscription when identity AND plan resolve unambiguously; ambiguous or
	// unresolvable findings stay requires_review.
	FindingRemoteSubMissingLocal FindingType = "pull.subscription.missing"
	// FindingLocalActiveRemoteDead (PS-2): local says active/past_due, the
	// rail says canceled/expired (on NMI: absent from the recurring
	// report). Enforce: cancel locally + revoke subscription-sourced
	// entitlements.
	FindingLocalActiveRemoteDead FindingType = "pull.subscription.dead"
	// FindingStatusMismatch (PS-3): statuses/periods disagree in a non-PS-2
	// way. Enforce: adopt the rail's status + period timestamps.
	FindingStatusMismatch FindingType = "pull.subscription.mismatch"
	// FindingChargeMissingLocal (PS-4): a successful rail charge has no
	// local payment record. Enforce: backfill billing.payments (+ entitlements
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
	// live remote subscriptions. Only the provider snapshot can see this
	// (local duplicates are schema-blocked), so it is a pull-plane finding.
	// Always requires_review: the fix (cancel+refund at the rail) is remote
	// and human.
	FindingDuplicateSubscriptions FindingType = "pull.subscription.duplicate"
	// FindingEvidenceStale: a terminal cancel was withheld because its
	// evidence predates this deployment's first pull of the merchant (or has
	// no date), so nothing we observed corroborates it. The row parks as
	// `unknown` with access intact. Always requires_review: only an operator
	// can say whether an imported record is true, and a withheld action must
	// be visible.
	//
	// Not in stateRosterFindingTypes: the unknown-cohort and webhook planes
	// write it too, so auto-resolving it on absence from a pull run would
	// erase their open findings.
	FindingEvidenceStale FindingType = "pull.subscription.evidence_stale"
	// FindingCancellationCapped: one pass planned more cancellations than the
	// merchant's per-pass budget allows, so none were applied and the pass
	// halted. Always requires_review: a book-sized cancellation is a human
	// decision.
	FindingCancellationCapped FindingType = "pull.cancellation.capped"
	// FindingReversalUnlinked: a refund or chargeback reached the charge
	// mirror (a declared book, a probe) without the sale it reverses. It is
	// never recorded as a charge; requires_review.
	FindingReversalUnlinked FindingType = "pull.reversal.unlinked"
	// FindingProviderScheduleDrift: a matched provider-owned schedule no
	// longer bills what the local subscription records — its amount, plan,
	// vault or paused state changed at the provider. Never acted on
	// automatically; requires_review.
	FindingProviderScheduleDrift FindingType = "pull.subscription.drift"
)

// The entitlement check (derive.grant_effect.mismatch) and stuck-intent check
// (life.provider_intent.stuck) belong to the Convergence Engine's DERIVE and
// LIFE passes; the pull engine emits pull.* findings only.

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
// (one-shot fetch+diff+apply; local writes only).
type Mode string

const (
	ModeAdvisory Mode = "advisory"
	ModeEnforce  Mode = "enforce"
)

// Finding is one diagnosed discrepancy as emitted by the diff engine, before
// persistence. SubjectKey is the stable identity within (provider, type), so
// re-runs update rather than duplicate.
type Finding struct {
	Provider Provider
	// PSPID is the PSP whose read raised a pull.* finding; part of its
	// identity.
	PSPID             uuid.UUID
	Type              FindingType
	SubjectKey        string
	Severity          Severity
	Status            FindingStatus // reconcile_required or requires_review at emit time
	RequiresAdmin     bool
	RecommendedAction string
	LocalEvidence     map[string]any
	RemoteEvidence    map[string]any
	// IntentEvidence is the local-intent annotation: when local state already
	// records the intent that explains the drift (e.g. DeletionScheduledAt set
	// => the recorded delete never executed), the finding documents it
	// instead of escalating to the admin queue.
	IntentEvidence map[string]any

	// Apply is the enforce instruction derived during the diff; nil when the
	// finding is advisory-only (requires_review types, intent-annotated drift).
	// Never persisted.
	Apply *ApplyAction `json:"-"`
}

// ApplyAction is one idempotent local write the enforce mode performs for a
// finding. Exactly one field is set; none ever touches a rail. Mirror writes
// (payments, refunds, vault metadata, subscription materialization) are direct
// appliers; subscription state transitions are a Decide action, since only the
// decider moves lifecycle state.
type ApplyAction struct {
	Decide             *DecideAction
	BackfillPayment    *BackfillPaymentAction
	RecordRefund       *RecordRefundAction
	AdoptPaymentMethod *AdoptPaymentMethodAction
	Materialize        *MaterializeSubscriptionAction
}

// DecideAction carries a decider transition computed at diff time from the
// snapshot evidence, applied through the engine's DecisionApplier under the
// mutation-policy gate.
type DecideAction struct {
	SubscriptionID uuid.UUID
	Decision       Decision
}

// MaterializeSubscriptionAction creates the local subscription for a PS-1
// finding whose identity and plan both resolved unambiguously. Identity comes
// from the engine's matcher (one vault/email match; zero or several keep the
// finding requires_review), the plan from catalog provider_links. The new
// subscription snapshots the product's specs like a normal signup, so
// entitlements flow through the subscription-sourced path.
type MaterializeSubscriptionAction struct {
	Provider Provider
	// PspID is billing.psps.id for the pull that materialized this row.
	// Required: subscriptions.psp_id is NOT NULL.
	PspID uuid.UUID
	// Rail is the LOCAL rail name to stamp on the subscription —
	// the key under which the price's provider link matched (e.g. "mobius",
	// "stripe"), so the new row joins the same roster future reconciles load.
	Rail               string
	RailSubscriptionID string
	CustomerID         uuid.UUID
	PriceID            uuid.UUID
	ProductID          uuid.UUID
	// Status is the canonical local lifecycle state the row is created with:
	// active or past_due, since PS-1 fires only for a live remote subscription.
	Status         models.SubscriptionStatus
	PeriodStartsAt *time.Time
	PeriodEndsAt   *time.Time
	StartedAt      *time.Time
	CustomerEmail  string
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
	SubscriptionID    uuid.UUID
	Created           bool
	AccessGranted     bool
	PaymentBackfilled bool
}

// BackfillPaymentAction inserts the missing local payment for a rail
// charge (PS-4), deduped on (tenant, rail, transaction_id), and grants
// the subscription's product when the period is current.
type BackfillPaymentAction struct {
	PspID         *uuid.UUID
	Rail          string
	TransactionID string
	AmountCents   int64
	// AmountMicros preserves exact host-ledger amounts for embedded historical
	// imports. Nil keeps the provider-wire cents conversion used by reconcile.
	AmountMicros   *int64
	Currency       string
	PurchasedAt    time.Time
	PriceID        uuid.UUID
	SubscriptionID *uuid.UUID
	CustomerID     uuid.UUID
	Metadata       map[string]any
	// Grant, when non-nil, grants the product for the current period after
	// the backfill (charge covers a period that is still running).
	Grant *GrantAccessAction
	// ChargeAfterCancel queues the backfilled charge's full refund with its
	// standing finding, in the backfill's transaction.
	ChargeAfterCancel bool
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
	// Card is the rail's record: its last four and expiry are adopted.
	Card models.Card
}

// GrantAccessAction grants a subscription's product for one window (PS-4
// current-period grant / PS-1 materialization).
type GrantAccessAction struct {
	SubscriptionID uuid.UUID
	CustomerID     uuid.UUID
	ProductID      uuid.UUID
	StartsAt       time.Time
	EndsAt         *time.Time
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
	FindingProviderScheduleDrift,
}

// SeverityRank orders severities worst-first (critical=0 .. low=3) for sorting
// and escalation (a FindingNotifier re-fires only when the rank strictly
// decreases).
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
