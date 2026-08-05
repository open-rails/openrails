package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// #665: the ONE subscription state-machine decider. Every plane — bulk pull
// snapshots, per-sub probes (unknown_probe.go), the LIFE sweep's charge/
// watermark evidence, and (#684) webhook-triggered fetches — produces an
// EvidenceBundle; only Decide maps (row, evidence) → transition, and only
// ApplyDecision moves state (through the shared lifecycle chokepoints).
// PURE — no DB, no IO — so the whole decision table is fixture-testable and
// scheduler ordering structurally cannot change outcomes (#664: an
// evidence-less bundle can only park).

// PeriodGrace dates the grace_ends_at PACING marker when a lapsed sub enters
// dunning, and is the detection debounce before an evidence-less lapsed row is
// parked as `unknown` (needs_verification). Since #691 it carries ZERO access
// stakes — an auto-renew sub's entitlement window is STANDING and closes only
// on proof, so this constant only paces state transitions. Evaluated against
// the cadence-derived collection.Window for that pacing role and
// deliberately kept: Decide is pure and price-less, and a pure internal
// debounce doesn't warrant threading price cadence through every plane.
const PeriodGrace = 48 * time.Hour

// renewalAlignmentSlack (#367 port): providers bill on their own day boundary, so
// the renewal charge for the next period can land up to a day BEFORE the local
// period-end instant. Charge classification and backfill both honor it.
const renewalAlignmentSlack = 24 * time.Hour

// DefaultDunningWindow bounds how far past the period end a FAILED renewal is
// still recoverable (past_due) vs terminal.
const DefaultDunningWindow = 14 * 24 * time.Hour

// SubscriptionState is the decider's view of the local row.
type SubscriptionState struct {
	Status             string // openrails.subscription_status
	Rail               string
	HasPaymentMethod   bool // payment_method_id IS NOT NULL
	RailSubscriptionID string
	PeriodEnd          *time.Time // current_period_ends_at
	GraceEndsAt        *time.Time
	NextRetryScheduled bool // next_retry_at IS NOT NULL
}

// ChargeEvidence is the first-party billing evidence (openrails.payments +
// dunning bookkeeping) a plane can attach.
type ChargeEvidence struct {
	// PaymentOpenedCurrentPeriod: a completed payment with purchased_at >=
	// current_period_starts_at — OpenRails billed (or observed) the current
	// period, so the rebill is ours to dun (#664 ownership leg).
	PaymentOpenedCurrentPeriod bool
	// RenewalPaymentAfterPeriodEnd: a completed payment at/after the lapsed
	// period end — billing DID happen; the renewal/advance path owns the row.
	RenewalPaymentAfterPeriodEnd bool
	// NonRetryableDecline: a RECORDED non-retryable decline for the current
	// period (#664 certainty leg). No sweep plane produces this today —
	// FailMembership decides inline at charge time — but the law encodes it.
	NonRetryableDecline bool
	// RetryAttempts / DunningMaxAttempts: real recorded dunning attempts vs the
	// policy max (#664 certainty leg: dunning genuinely exhausted, never
	// merely "grace elapsed").
	RetryAttempts      int
	DunningMaxAttempts int
	// LastAttemptAt DATES the two certainty legs above — the instant of the
	// recorded non-retryable decline / final dunning attempt. It is what the
	// #835 staleness floor measures a first-party cancel against.
	//
	// NO plane populates it today, because no plane populates the legs either
	// (FailMembership decides inline at charge time). That makes first-party
	// certainty UNDATED, and the floor refuses undated evidence rather than
	// assuming it is fresh: a plane that starts producing these legs must date
	// them in the same change.
	LastAttemptAt time.Time
}

func (c ChargeEvidence) dunningExhausted() bool {
	return c.DunningMaxAttempts > 0 && c.RetryAttempts >= c.DunningMaxAttempts
}

// certaintyLeg names the #664 certainty leg this evidence carries, or "".
// These are the ONLY first-party justifications for a terminal cancel (#821).
func (c ChargeEvidence) certaintyLeg() string {
	switch {
	case c.NonRetryableDecline:
		return collection.CertaintyNonRetryableDecline
	case c.dunningExhausted():
		return collection.CertaintyDunningExhausted
	default:
		return ""
	}
}

// EvidenceBundle unifies what the planes produce (#665): provider snapshots
// (pull / probe / webhook fetch — coverage-absence proof rides in
// Snapshot.Coverage), first-party charge evidence, and watermark freshness.
// The decider consumes ONLY this, so its inputs are testable as data.
type EvidenceBundle struct {
	// Snapshot is provider truth. Nil = no provider fetch happened.
	Snapshot *RemoteSnapshot
	Charge   ChargeEvidence
	// WatermarkNewerThanPeriodEnd: the rail refresh watermark for the sub's
	// (provider, account) is newer than the lapsed period end — provider truth
	// synced since the lapse and saw no renewal (#664 ownership leg).
	WatermarkNewerThanPeriodEnd bool
	// EvidenceFloor (#835) is the instant this deployment first completed a
	// provider pull for this merchant
	// (openrails.merchant_destructive_policy.first_pull_completed_at). Evidence
	// OLDER than it was never corroborated by an observation this deployment
	// made: on an imported legacy book it is inherited history, and inherited
	// history is exactly what the arming gate could not protect against once an
	// operator armed enforcement without reading the advisory findings.
	//
	// ZERO means no completed pull is on record. Nothing on file is then
	// corroborated, so the floor falls back to THIS pass's own observation
	// (Snapshot.FetchedAt) — a caller that forgets to supply the floor gets the
	// STRICTER answer, never a permissive one. With neither a recorded first
	// pull nor a dated snapshot there is nothing to measure against and the
	// floor is inert; the only plane in that position (LIFE, which carries no
	// snapshot) cannot reach a cancel at all.
	//
	// The declared import (#737) deliberately leaves it zero: its snapshot is
	// dated at the operator's AsOf horizon, and the operator's declaration IS
	// the observation.
	EvidenceFloor time.Time
}

// evidenceFloor is the instant before which this bundle can vouch for nothing.
func (ev EvidenceBundle) evidenceFloor() time.Time {
	if !ev.EvidenceFloor.IsZero() {
		return ev.EvidenceFloor
	}
	if ev.Snapshot != nil {
		return ev.Snapshot.FetchedAt
	}
	return time.Time{}
}

// TransitionKind is the decider's transition vocabulary.
type TransitionKind int

const (
	// TransitionNone: no evidence-justified move (incl. "stay unknown").
	TransitionNone TransitionKind = iota
	// TransitionRenew: a VERIFIED successful renewal charge exists → active
	// with a renewal-shaped period advance. The ONLY transition that extends
	// access (#367 doctrine: entitlements extend only through a real charge).
	TransitionRenew
	// TransitionAdoptPeriodEnd (#367): roster alive with a FUTURE next billing
	// and NO charge — re-anchor the period END only; adoption alone never
	// grants access (start untouched → DERIVE projects nothing).
	TransitionAdoptPeriodEnd
	// TransitionPastDue: evidence the rebill is ours (or provider-declared
	// failure still within the dunning window) → dunning.
	TransitionPastDue
	// TransitionParkUnknown: no evidence → park (access intact), provider
	// verification resolves it. The ONLY transition an evidence-less bundle
	// can produce (#664).
	TransitionParkUnknown
	// TransitionCancel: terminal, certainty only — provider-confirmed dead,
	// a non-retryable decline, or dunning genuinely exhausted.
	TransitionCancel
)

func (k TransitionKind) String() string {
	switch k {
	case TransitionRenew:
		return "renew"
	case TransitionAdoptPeriodEnd:
		return "adopt_period_end"
	case TransitionPastDue:
		return "past_due"
	case TransitionParkUnknown:
		return "park_unknown"
	case TransitionCancel:
		return "cancel"
	default:
		return "none"
	}
}

// Decision is one decider transition plus the evidence-derived side data.
type Decision struct {
	Kind TransitionKind
	// NewPeriodEnd is the provider-confirmed next period end
	// (TransitionRenew / TransitionAdoptPeriodEnd).
	NewPeriodEnd *time.Time
	// GraceEndsAt dates the dunning grace window (TransitionPastDue): the
	// missed period end + PeriodGrace — one rule for every plane.
	GraceEndsAt time.Time
	// RemoteGone (#679, TransitionCancel): the provider-side subscription is
	// confirmed gone (roster cancelled/expired, or absent from an exhaustive
	// roster). FALSE for the stale-decline cancel, where the remote sub may
	// still exist and keep retrying — the apply path then queues the deferred
	// provider delete.
	RemoteGone bool
	// Backfill are the provider's charge-level events for this subscription
	// at/after the lapsed period end (#634) — the missing payments to import,
	// INCLUDING declines/voids (recorded as failed). Populated whenever a
	// snapshot was consulted, even on TransitionNone.
	Backfill []RemoteTransaction
	// RemoteCustomerID is the provider's customer-scope object id when it has
	// one (Stripe cus_*; NMI reports the per-card vault id — #682, so only
	// Stripe materializes rail_customer_accounts).
	RemoteCustomerID string
	// Certainty (#821) names the leg that justified TransitionCancel — one of
	// the Certainty* constants. A TransitionCancel with an EMPTY Certainty is
	// structurally impossible: Decide downgrades it to TransitionParkUnknown.
	Certainty string
	// EvidenceAt (#835) DATES the observation that justified the transition —
	// the instant the staleness floor measures against. Zero = the leg carries
	// no timestamp, which the floor treats as undated, never as fresh.
	EvidenceAt time.Time
	// EvidenceFloored (#835) records that the staleness floor downgraded a
	// TransitionCancel to a park: the supporting evidence predates this
	// deployment's first pull of the merchant (or carries no date at all). The
	// planes turn it into an operator finding — a floored row must be visible,
	// not a silent no-op.
	EvidenceFloored bool
	// Reason is a short cause slug for finding evidence / logs.
	Reason string
}

// Decide maps (current row, evidence bundle) → transition. PURE. dunningWindow
// bounds how far past the period end a provider-declared failure is still
// recoverable; <=0 uses DefaultDunningWindow.
//
// Law, in evidence order:
//  1. Provider snapshot (when present and the row has a provider handle)
//     decides conclusively: verified renewal charge → renew; roster alive with
//     future boundary → adopt end; declared failure → past_due within the
//     window, cancel beyond; roster dead / absent-from-exhaustive → cancel.
//  2. First-party evidence (the #664 LIFE law): a lapsed active row with
//     ownership evidence (payment opened period, or watermark newer than the
//     period end) on an ours-to-bill rail → dunning; with a renewal payment
//     recorded → no-op (the advance path owns it); otherwise parked after
//     PeriodGrace. A stalled past_due row (grace elapsed, no retry) parks
//     unless a certainty leg (non-retryable decline / dunning exhausted)
//     justifies the terminal cancel.
//  3. Nothing → no-op. An `unknown` row without a snapshot stays unknown.
//
// Every cancel-shaped outcome then passes ONE chokepoint (gateCancelCertainty →
// gateEvidenceFloor): the evidence must be of a kind that proves death (#821)
// AND dated at/after this deployment's first pull of the merchant (#835).
// Anything else parks as `unknown` with access intact.
func Decide(sub SubscriptionState, ev EvidenceBundle, now time.Time, dunningWindow time.Duration) Decision {
	if dunningWindow <= 0 {
		dunningWindow = DefaultDunningWindow
	}
	switch sub.Status {
	case string(models.StatusActive), string(models.StatusPastDue), string(models.StatusUnknown):
	default:
		return Decision{Kind: TransitionNone, Reason: "terminal_or_pending_status"}
	}

	// The lapsed period boundary the snapshot law anchors on.
	periodEnd := now
	if sub.PeriodEnd != nil {
		periodEnd = *sub.PeriodEnd
	}

	// Stage 1: provider truth.
	var carried Decision // backfill/customer-id survive an inconclusive snapshot
	if ev.Snapshot != nil && sub.RailSubscriptionID != "" {
		d := decideFromSnapshot(sub.RailSubscriptionID, periodEnd, ev.Snapshot, now, dunningWindow)
		if d.Kind != TransitionNone {
			return gateCancelCertainty(d, ev)
		}
		carried = d
	}

	// Stage 2: first-party charge + freshness evidence.
	d := decideFromFirstParty(sub, ev, now)
	d.Backfill = carried.Backfill
	if d.RemoteCustomerID == "" {
		d.RemoteCustomerID = carried.RemoteCustomerID
	}
	if d.Reason == "" {
		d.Reason = carried.Reason
	}
	return gateCancelCertainty(d, ev)
}

// gateCancelCertainty is the #821 chokepoint: NO plane may reach
// TransitionCancel — and the irreversible provider-side delete it queues —
// without a named certainty leg. Certainty is either the provider's own word
// (RemoteGone: the roster says dead, or the row is absent from a PROVEN
// exhaustive roster) or first-party proof (a non-retryable decline, or dunning
// genuinely exhausted). A date comparison is not evidence: NMI rebills
// forever, so a lapsed next_billing_date is the NORMAL state of every dunning
// customer, and an absence of data is not a death certificate. Without
// certainty the row PARKS as `unknown` — access intact — and a targeted
// per-subscription probe resolves it.
func gateCancelCertainty(d Decision, ev EvidenceBundle) Decision {
	if d.Kind != TransitionCancel {
		return d
	}
	switch {
	case d.Certainty != "":
	case d.RemoteGone:
		d.Certainty = collection.CertaintyProviderConfirmedDead
	default:
		d.Certainty = ev.Charge.certaintyLeg()
	}
	if d.Certainty == "" {
		return parkCancel(d, "_no_certainty", false)
	}
	return gateEvidenceFloor(d, ev)
}

// gateEvidenceFloor is the #835 staleness floor and the SECOND half of the one
// destructive chokepoint: it is reachable only from gateCancelCertainty, so no
// plane can pass the certainty gate and skip the floor.
//
// Certainty says the evidence is the right KIND. The floor says the evidence is
// ours: a destructive action may not rest on a record that predates the first
// pull this deployment ever completed for the merchant. Such a record was never
// corroborated by anything we observed — on an imported legacy book it is
// inherited history that arrived with the data — and "no evidence, no action"
// applies to inherited evidence exactly as it applies to missing evidence. The
// arming gate covers the FIRST pass; this covers every pass after it, which is
// the failure that survives an operator arming enforcement before reading the
// advisory findings carefully.
//
// It gates the terminal cancel + entitlement revoke and the irreversible
// provider delete they queue — nothing else. Reads, findings persistence,
// renewals, period adoption, dunning entry and parking are untouched.
func gateEvidenceFloor(d Decision, ev EvidenceBundle) Decision {
	floor := ev.evidenceFloor()
	if floor.IsZero() {
		// Neither a recorded first pull nor a dated observation: there is
		// nothing to measure against. Unreachable for a cancel on any wired
		// plane — all of them either carry the merchant's floor or a dated
		// provider snapshot.
		return d
	}
	at := cancelEvidenceAt(d, ev)
	switch {
	case at.IsZero():
		return parkCancel(d, "_evidence_undated", true)
	case at.Before(floor):
		return parkCancel(d, "_evidence_predates_first_pull", true)
	}
	d.EvidenceAt = at
	return d
}

// cancelEvidenceAt dates the certainty leg that justified this cancel:
//
//	provider-confirmed dead  the roster read THIS pass performed — the
//	                         provider's current word, whatever leg was named
//	non-retryable decline    the declined attempt's OccurredAt (provider
//	                         snapshot) or the recorded attempt (first-party)
//	dunning exhausted        the final recorded dunning attempt
func cancelEvidenceAt(d Decision, ev EvidenceBundle) time.Time {
	if d.RemoteGone && ev.Snapshot != nil {
		return ev.Snapshot.FetchedAt
	}
	if !d.EvidenceAt.IsZero() {
		return d.EvidenceAt
	}
	return ev.Charge.LastAttemptAt
}

// parkCancel downgrades a refused cancel to the park every ungated transition
// falls back to: access intact, provider verification resolves it. The reason
// suffix names which half of the chokepoint refused it.
func parkCancel(d Decision, suffix string, floored bool) Decision {
	return Decision{
		Kind:             TransitionParkUnknown,
		Backfill:         d.Backfill,
		RemoteCustomerID: d.RemoteCustomerID,
		EvidenceAt:       d.EvidenceAt,
		EvidenceFloored:  floored,
		Reason:           d.Reason + suffix,
	}
}

// EvidenceFloorFor reads the merchant's #835 staleness floor. Every plane that
// can reach a destructive transition loads it HERE, from the merchant's own
// destructive policy, instead of accepting it from a caller — a floor that has
// to be passed in is a floor a call site can forget (or#842: a gate enforced at
// exactly one site while the other paths walked around it). ctx must be
// merchant-scoped.
func EvidenceFloorFor(ctx context.Context, database *db.DB, merchantID uuid.UUID) time.Time {
	return destructive.New(database).EvidenceFloor(ctx, merchantID)
}

// decideFromSnapshot is the provider-truth law (#632/#633 resolution core,
// generalized from ResolveUnknownFromSnapshot). TransitionNone = inconclusive.
func decideFromSnapshot(railSubID string, periodEnd time.Time, snap *RemoteSnapshot, now time.Time, dunningWindow time.Duration) Decision {
	// This subscription's charge events + roster entry from the pull.
	var txns []RemoteTransaction
	for i := range snap.Transactions {
		if snap.Transactions[i].SubscriptionID == railSubID {
			txns = append(txns, snap.Transactions[i])
		}
	}
	var remoteSub *RemoteSubscription
	for i := range snap.Subscriptions {
		if snap.Subscriptions[i].RailSubscriptionID == railSubID {
			remoteSub = &snap.Subscriptions[i]
			break
		}
	}
	base := Decision{Kind: TransitionNone, Reason: "snapshot_inconclusive"}
	if remoteSub != nil {
		// Carry the provider's customer-scope id so the orchestration can
		// materialize a rail_customer_accounts row (#635).
		base.RemoteCustomerID = remoteSub.CustomerID
	}

	// The renewal for the next period can land up to renewalAlignmentSlack before
	// the local period-end instant (provider day boundaries); classify and
	// backfill from that floor so the aligned renewal charge is never dropped.
	chargeCutoff := periodEnd.Add(-renewalAlignmentSlack)
	base.Backfill = subscriptionBackfill(txns, chargeCutoff)

	// Latest successful renewal vs latest decline at/after the aligned cutoff.
	var renewTxn, declineTxn *RemoteTransaction
	for i := range txns {
		t := &txns[i]
		if t.OccurredAt.Before(chargeCutoff) {
			continue
		}
		switch {
		case t.Type == TransactionTypeSale && t.Success:
			if renewTxn == nil || t.OccurredAt.After(renewTxn.OccurredAt) {
				renewTxn = t
			}
		case t.Type == TransactionTypeDecline || (t.Type == TransactionTypeSale && !t.Success):
			if declineTxn == nil || t.OccurredAt.After(declineTxn.OccurredAt) {
				declineTxn = t
			}
		}
	}

	with := func(d Decision) Decision {
		d.Backfill = base.Backfill
		d.RemoteCustomerID = base.RemoteCustomerID
		return d
	}

	// A roster-declared dead sub is provider certainty: it will never bill
	// again, so a renewal charge cannot resurrect it (the charge is still
	// backfilled — money truth is separate from lifecycle truth).
	rosterDead := remoteSub != nil &&
		(remoteSub.Status == SubscriptionStatusCancelled || remoteSub.Status == SubscriptionStatusExpired)

	// 1) A VERIFIED successful renewal charge → the provider billed the new
	//    period. The only renewal-shaped outcome (#367: renew only off a real charge).
	if renewTxn != nil && !rosterDead {
		return with(Decision{Kind: TransitionRenew, NewPeriodEnd: remoteNextEnd(remoteSub), Reason: "verified_renewal_charge"})
	}
	// 2) Roster alive with a FUTURE boundary but no charge → adopt the provider's
	//    clock (#367: period adoption alone never grants access). Active with a
	//    stale/absent boundary falls through to the failure evidence below; with
	//    none, the row waits until the provider moves.
	if remoteSub != nil && remoteSub.Status == SubscriptionStatusActive {
		if next := remoteSub.NextBillingAt; next != nil && next.After(now) {
			return with(Decision{Kind: TransitionAdoptPeriodEnd, NewPeriodEnd: next, Reason: "roster_alive_future_boundary"})
		}
	}
	// remoteGone (#679): the provider-side sub is confirmed gone — roster says
	// cancelled/expired, or it is absent from an exhaustive roster.
	remoteGone := false
	if remoteSub != nil {
		remoteGone = remoteSub.Status == SubscriptionStatusCancelled || remoteSub.Status == SubscriptionStatusExpired
	} else {
		remoteGone = snap.Coverage.SubscriptionsExhaustive
	}

	// 3) A failed/declined renewal: past_due if still recoverable, else terminal
	//    — but ONLY when the decline itself is certainty (#821). A soft decline
	//    (NSF, do-not-honor, comms) beyond the window is a customer still in
	//    dunning on a schedule the rail keeps retrying, not a dead one; it falls
	//    through gateCancelCertainty and parks.
	if declineTxn != nil {
		if now.Sub(periodEnd) <= dunningWindow {
			return with(Decision{Kind: TransitionPastDue, GraceEndsAt: periodEnd.Add(PeriodGrace), Reason: "declined_renewal_within_window"})
		}
		// #835: the decline attempt itself dates this cancel. On an imported
		// book the decline arrived with the data and can be years older than
		// anything this deployment observed.
		d := Decision{Kind: TransitionCancel, RemoteGone: remoteGone, EvidenceAt: declineTxn.OccurredAt, Reason: "declined_renewal_beyond_window"}
		if collection.ClassifyDecline(string(snap.Provider), declineTxn.DeclineCode) == collection.DeclineNonRecoverable {
			d.Certainty = collection.CertaintyNonRetryableDecline
		}
		return with(d)
	}
	// 4) The roster says the renewal stalled/failed (NMI next-charge wedged in
	//    the past, Stripe past_due/unpaid) — recoverable inside the window.
	//    BEYOND the window there is still NO evidence of death: the remote
	//    record exists and NMI rebills forever, so a lapsed next_billing_date is
	//    the normal state of every dunning customer on an imported book (#821).
	//    Emit the cancel-shaped decision and let gateCancelCertainty decide: it
	//    survives only on a first-party certainty leg, otherwise it parks.
	if remoteSub != nil && remoteSub.Status == SubscriptionStatusPastDue {
		if now.Sub(periodEnd) <= dunningWindow {
			return with(Decision{Kind: TransitionPastDue, GraceEndsAt: periodEnd.Add(PeriodGrace), Reason: "roster_past_due_within_window"})
		}
		return with(Decision{Kind: TransitionCancel, RemoteGone: remoteGone, Reason: "roster_past_due_beyond_window"})
	}
	// 5) The provider says cancelled/expired. The evidence is the roster read
	//    itself, so it dates from this pass (#835).
	if remoteSub != nil && (remoteSub.Status == SubscriptionStatusCancelled || remoteSub.Status == SubscriptionStatusExpired) {
		return with(Decision{Kind: TransitionCancel, RemoteGone: true, EvidenceAt: snap.FetchedAt, Reason: "roster_dead"})
	}
	// 6) The sub is absent AND the pull exhaustively covered subscriptions → it
	//    was deleted at the provider (coverage-absence proof).
	if remoteSub == nil && snap.Coverage.SubscriptionsExhaustive {
		return with(Decision{Kind: TransitionCancel, RemoteGone: true, EvidenceAt: snap.FetchedAt, Reason: "absent_from_exhaustive_roster"})
	}
	// 7) No conclusive provider evidence.
	return base
}

// decideFromFirstParty is the #664 LIFE law over charge + watermark evidence.
func decideFromFirstParty(sub SubscriptionState, ev EvidenceBundle, now time.Time) Decision {
	switch sub.Status {
	case string(models.StatusActive):
		if sub.PeriodEnd == nil || !sub.PeriodEnd.Before(now) {
			return Decision{Kind: TransitionNone}
		}
		// Rail heuristics survive only as a negative signal (#664): ccbill /
		// vault-less nmi / stripe / solana are provider-auto-billed, never ours
		// to charge. The positive "ours" signal is evidence, never rail.
		oursToBill := sub.Rail == string(models.RailNMI) && sub.HasPaymentMethod
		ownership := ev.Charge.PaymentOpenedCurrentPeriod || ev.WatermarkNewerThanPeriodEnd
		if oursToBill && ownership {
			return Decision{Kind: TransitionPastDue, GraceEndsAt: sub.PeriodEnd.Add(PeriodGrace), Reason: "period_overdue_ownership_evidence"}
		}
		if ev.Charge.RenewalPaymentAfterPeriodEnd {
			// Billing DID happen — the renewal/advance path owns the row.
			return Decision{Kind: TransitionNone, Reason: "renewal_payment_recorded"}
		}
		if now.Sub(*sub.PeriodEnd) > PeriodGrace {
			return Decision{Kind: TransitionParkUnknown, Reason: "no_ownership_evidence"}
		}
		return Decision{Kind: TransitionNone, Reason: "within_grace_slack"}

	case string(models.StatusPastDue):
		graceElapsed := sub.GraceEndsAt != nil && sub.GraceEndsAt.Before(now)
		if graceElapsed && !sub.NextRetryScheduled {
			// #664 certainty legs. No sweep plane produces them today
			// (FailMembership owns the inline decision), so convergence still
			// only ever parks — structurally.
			if leg := ev.Charge.certaintyLeg(); leg != "" {
				// #835: dated by the recorded attempt. Nothing populates
				// LastAttemptAt today, so this leg is undated and the floor
				// refuses it — see ChargeEvidence.LastAttemptAt.
				return Decision{Kind: TransitionCancel, Certainty: leg, EvidenceAt: ev.Charge.LastAttemptAt, Reason: "dunning_exhausted_certainty"}
			}
			return Decision{Kind: TransitionParkUnknown, Reason: "dunning_stalled_past_grace"}
		}
		return Decision{Kind: TransitionNone}

	default: // unknown without a conclusive snapshot stays unknown
		return Decision{Kind: TransitionNone, Reason: "awaiting_provider_verification"}
	}
}

// remoteNextEnd is the provider's declared next billing time, or nil.
func remoteNextEnd(s *RemoteSubscription) *time.Time {
	if s == nil {
		return nil
	}
	return s.NextBillingAt
}

// subscriptionBackfill returns the charge events for one subscription at/after the
// lapsed period end (#634): the candidate missing payments — successes AND
// declines/voids — for the orchestration to import idempotently by transaction id.
func subscriptionBackfill(txns []RemoteTransaction, since time.Time) []RemoteTransaction {
	var out []RemoteTransaction
	for i := range txns {
		if !txns[i].OccurredAt.Before(since) {
			out = append(out, txns[i])
		}
	}
	return out
}

// ApplyDecision routes one decider transition through the shared subscription
// lifecycle chokepoints — the ONLY way a plane moves subscription state (#665).
// Non-unknown rows take the `unknown` waypoint (park, then resolve) so every
// transition lands through the one resolution implementation; each leg is
// idempotent, so a crash between them is healed by the next pass. Returns
// whether a transition was attempted (false = no-op / precondition unmet).
func ApplyDecision(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, sub *models.Subscription, d Decision, now time.Time) (bool, error) {
	if database == nil || lc == nil || sub == nil {
		return false, fmt.Errorf("apply decision: db, lifecycle and subscription are required")
	}
	switch d.Kind {
	case TransitionNone:
		return false, nil

	case TransitionParkUnknown:
		if sub.Status != models.StatusActive && sub.Status != models.StatusPastDue {
			return false, nil
		}
		return true, lc.ApplyLocalUnknown(ctx, database, sub)

	case TransitionPastDue:
		if sub.Status == models.StatusUnknown {
			return true, lc.ResolveUnknownSubscription(ctx, database, sub, subscriptions.ResolvePastDue, nil, d.GraceEndsAt)
		}
		if sub.Status != models.StatusActive {
			return false, nil
		}
		if sub.CurrentPeriodEndsAt == nil {
			return false, nil // chk_past_due_has_period_end: unsatisfiable; park path owns it
		}
		return true, lc.ApplyLocalPastDue(ctx, database, sub, d.GraceEndsAt)

	case TransitionRenew, TransitionAdoptPeriodEnd, TransitionCancel:
		if sub.Status != models.StatusUnknown {
			if sub.Status != models.StatusActive && sub.Status != models.StatusPastDue {
				return false, nil
			}
			// `unknown` waypoint: one resolution implementation for every plane.
			if err := lc.ApplyLocalUnknown(ctx, database, sub); err != nil {
				return false, err
			}
		}
		var res subscriptions.UnknownResolution
		switch d.Kind {
		case TransitionRenew:
			res = subscriptions.ResolveRenewed
		case TransitionAdoptPeriodEnd:
			res = subscriptions.ResolveAdopted
		default:
			res = subscriptions.ResolveCancelled
			if !d.RemoteGone {
				// #679: the remote sub may still exist and keep retrying; the
				// lifecycle queues the deferred provider delete.
				res = subscriptions.ResolveCancelledRemoteAlive
			}
		}
		grace := d.GraceEndsAt
		if grace.IsZero() {
			grace = now
		}
		return true, lc.ResolveUnknownSubscription(ctx, database, sub, res, d.NewPeriodEnd, grace)

	default:
		return false, fmt.Errorf("apply decision: unknown transition %d", d.Kind)
	}
}

// DecisionApplier applies decider transitions for the pull engine's enforce
// path. Fakeable for unit tests.
type DecisionApplier interface {
	ApplyDecision(ctx context.Context, subscriptionID uuid.UUID, d Decision) (bool, error)
}

// LifecycleDecisionApplier is the production DecisionApplier: load the row,
// route through ApplyDecision on the same merchant-scoped handle.
type LifecycleDecisionApplier struct {
	DB *db.DB
	LC *subscriptions.SubscriptionLifecycleService
}

// NewDecisionApplier builds the production applier. deferDelete may be nil
// (CLI pulls): a stale-decline cancel then logs the wiring gap instead of
// queuing the deferred NMI delete.
func NewDecisionApplier(database *db.DB, deferDelete subscriptions.DeferredDeleteScheduler) *LifecycleDecisionApplier {
	lc := subscriptions.NewSubscriptionLifecycleService(database, nil, nil, nil, nil, nil, nil, clockwork.NewRealClock())
	if deferDelete != nil {
		lc.SetDeferredDeleteScheduler(deferDelete)
	}
	return &LifecycleDecisionApplier{DB: database, LC: lc}
}

func (a *LifecycleDecisionApplier) ApplyDecision(ctx context.Context, subscriptionID uuid.UUID, d Decision) (bool, error) {
	sub, err := subscriptions.NewSubscriptionRepo(a.DB).GetByID(ctx, subscriptionID)
	if err != nil {
		return false, fmt.Errorf("apply decision: load subscription %s: %w", subscriptionID, err)
	}
	return ApplyDecision(ctx, a.DB, a.LC, sub, d, time.Now().UTC())
}
