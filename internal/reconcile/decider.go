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
	"github.com/open-rails/openrails/internal/shared/timeutil"
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

// AlignmentSlack is the renewal alignment window for a period: a day, but
// never more than half the period, so a short (daily) cycle's PREVIOUS charge
// can never pass for this period's renewal. Unknown bounds use the day.
func AlignmentSlack(start, end *time.Time) time.Duration {
	if start == nil || end == nil || !end.After(*start) {
		return renewalAlignmentSlack
	}
	return min(renewalAlignmentSlack, end.Sub(*start)/2)
}

// boundaryAdvanced reports that a provider's next billing date lies at least
// half a period (a day when the period is unknown) beyond the local period
// end: the provider billed, or tried to bill, a period this row never saw.
func boundaryAdvanced(next time.Time, start, end *time.Time) bool {
	if end == nil {
		return false
	}
	gap := renewalAlignmentSlack
	if start != nil && end.After(*start) {
		gap = end.Sub(*start) / 2
	}
	return next.Sub(*end) >= gap
}

// DefaultDunningWindow bounds how far past the period end a FAILED renewal is
// still recoverable (past_due) vs terminal.
const DefaultDunningWindow = 14 * 24 * time.Hour

// SubscriptionState is the decider's view of the local row.
type SubscriptionState struct {
	CollectionPolicy   models.CollectionPolicy
	Status             string // openrails.subscription_status
	Rail               string
	RailSubscriptionID string
	PeriodStart        *time.Time // current_period_starts_at: bounds the period's cadence
	PeriodEnd          *time.Time // current_period_ends_at
	GraceEndsAt        *time.Time
	NextRetryScheduled bool // next_retry_at IS NOT NULL
}

// ChargeEvidence is the first-party billing evidence (openrails.payments +
// dunning bookkeeping) a plane can attach.
type ChargeEvidence struct {
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
	// NewPeriodStart is the provider-stated start of that period, when known.
	NewPeriodStart *time.Time
	// GraceEndsAt dates the dunning grace window (TransitionPastDue):
	// PeriodGrace after the missed period end, or after a decline is seen
	// when that is later.
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
	// Decline is the provider's declined renewal behind a TransitionPastDue,
	// when one was seen: the attempt OpenRails dunning counts and classifies.
	Decline *RemoteTransaction
	// Reason is a short cause slug for finding evidence / logs.
	Reason string
	// DecidedStatus and DecidedPeriodEnd are the row the decision was made
	// on. ApplyDecision drops the decision when the row has since moved: a
	// renewal or decline that landed during a pull must not be overwritten.
	DecidedStatus    string
	DecidedPeriodEnd *time.Time
	// Silent sends no customer notices: a declared import replays history.
	Silent bool
}

// DunsDecline reports that OpenRails' dunning owns the retries after this
// decision's decline: a provider_dunning NMI schedule entering past_due off a
// seen decline. FailMembership then counts, classifies and schedules it.
func DunsDecline(sub *models.Subscription, d Decision) bool {
	return d.Kind == TransitionPastDue && d.Decline != nil && sub != nil && sub.CollectionPolicy == models.CollectionPolicyProviderDunning
}

// Decide maps (current row, evidence bundle) → transition. PURE. dunningWindow
// bounds how far past the period end a provider-declared failure is still
// recoverable; <=0 uses DefaultDunningWindow.
//
// Law, in evidence order:
//  1. Provider snapshot (when present and the row has a provider handle)
//     decides conclusively: verified renewal charge → renew; roster alive with
//     future boundary without a decline → adopt end (never lifting past_due); declared failure → past_due within the
//     window, cancel beyond; roster dead / absent-from-exhaustive → cancel.
//  2. First-party evidence (the #664 LIFE law): a lapsed active row with a
//     renewal payment recorded → no-op (the advance path owns it); otherwise
//     parked after PeriodGrace. A lapse is never dunning: only a seen decline
//     opens it (the provider may have billed, unobserved). A stalled past_due row (grace elapsed, no retry) parks
//     unless a certainty leg (non-retryable decline / dunning exhausted)
//     justifies the terminal cancel.
//  3. Nothing → no-op. An `unknown` row without a snapshot stays unknown.
//
// Every cancel-shaped outcome then passes ONE chokepoint (gateCancelCertainty →
// gateEvidenceFloor): the evidence must be of a kind that proves death (#821)
// AND dated at/after this deployment's first pull of the merchant (#835).
// Anything else parks as `unknown` with access intact.
func Decide(sub SubscriptionState, ev EvidenceBundle, now time.Time, dunningWindow time.Duration) Decision {
	d := decide(sub, ev, now, dunningWindow)
	d.DecidedStatus, d.DecidedPeriodEnd = sub.Status, sub.PeriodEnd
	return d
}

func decide(sub SubscriptionState, ev EvidenceBundle, now time.Time, dunningWindow time.Duration) Decision {
	if dunningWindow <= 0 {
		dunningWindow = DefaultDunningWindow
	}
	switch sub.Status {
	case string(models.StatusActive), string(models.StatusPastDue), string(models.StatusAwaitingMethod), string(models.StatusUnverified):
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
		d := decideFromSnapshot(sub.RailSubscriptionID, sub.PeriodStart, sub.PeriodEnd, periodEnd, ev.Snapshot, now, dunningWindow)
		if d.Kind == TransitionAdoptPeriodEnd && sub.Status == string(models.StatusPastDue) {
			// A recorded decline stands until a verified charge renews the row.
			// A future provider date is not payment: NMI advances it on a decline too.
			d.Kind, d.NewPeriodEnd, d.NewPeriodStart, d.Reason = TransitionNone, nil, nil, "past_due_awaits_verified_charge"
		}
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
func decideFromSnapshot(railSubID string, localStart, localEnd *time.Time, periodEnd time.Time, snap *RemoteSnapshot, now time.Time, dunningWindow time.Duration) Decision {
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
	chargeCutoff := periodEnd.Add(-AlignmentSlack(localStart, localEnd))
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
		if declineTxn != nil && declineTxn.OccurredAt.After(renewTxn.OccurredAt) {
			// A later decline: NMI's next date runs past the unpaid period.
			// Renew only the periods the charges paid; the decline then dunns.
			return with(renewPaidPeriods(txns, chargeCutoff, declineTxn.OccurredAt, localStart, localEnd, base))
		}
		start, end := paidPeriod(txns, chargeCutoff, localStart, localEnd, remoteSub)
		if end == nil {
			base.Reason = "renewal_period_unknown"
			return base
		}
		return with(Decision{Kind: TransitionRenew, NewPeriodStart: start, NewPeriodEnd: end, Reason: "verified_renewal_charge"})
	}
	// 2) Roster alive with a FUTURE boundary but no charge → adopt the provider's
	//    clock (#367: period adoption alone never grants access). A future
	//    scheduled date cannot overrule a verified decline: NMI moves to the
	//    next regular date even when the current charge fails. Leave that
	//    unpaid period eligible for the failure handling below.
	if declineTxn == nil && remoteSub != nil && remoteSub.Status == SubscriptionStatusActive {
		if next := remoteSub.NextBillingAt; next != nil && next.After(now) {
			if renewTxn == nil && snap.Provider == ProviderNMI && boundaryAdvanced(*next, localStart, localEnd) {
				// The provider moved past a boundary this snapshot cannot
				// explain (NMI's bulk transaction report carries no schedule
				// id). Adopting would skip a period without its charge; a
				// per-subscription probe decides it on real evidence.
				base.Reason = "roster_advanced_without_charge_evidence"
				return base
			}
			return with(Decision{Kind: TransitionAdoptPeriodEnd, NewPeriodEnd: next, NewPeriodStart: remoteSub.PeriodStart, Reason: "roster_alive_future_boundary"})
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
	// Stripe runs its own retries: a past_due subscription is one Stripe is
	// still retrying (a spent schedule maps to expired), whatever its age.
	if snap.Provider == ProviderStripe && remoteSub != nil && remoteSub.Status == SubscriptionStatusPastDue {
		return with(Decision{Kind: TransitionPastDue, GraceEndsAt: laterOf(periodEnd, now).Add(PeriodGrace), Decline: declineTxn, Reason: "stripe_retrying"})
	}
	if declineTxn != nil {
		if now.Sub(periodEnd) <= dunningWindow {
			// Grace runs from when the decline is seen: one found late still
			// gets its whole window.
			return with(Decision{Kind: TransitionPastDue, GraceEndsAt: laterOf(periodEnd, now).Add(PeriodGrace), Decline: declineTxn, Reason: "declined_renewal_within_window"})
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
		if periodEnd.After(now) && snap.Provider != ProviderDeclared {
			// A paid-through row: OpenRails' own recovery charge does not move
			// NMI's date, so a stale roster date is no failure of this period.
			// A declared book's dunning state is the operator's own word.
			base.Reason = "roster_past_due_within_paid_period"
			return base
		}
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
	// Engine cadence and uncertain recovery belong to the accepted operation.
	// Native roster freshness and a saved card cannot supply that ownership.
	// Explicit terminal charge evidence still passes the shared certainty gates.
	if sub.CollectionPolicy == models.CollectionPolicyEngine && (sub.Status != string(models.StatusPastDue) || ev.Charge.certaintyLeg() == "") {
		return Decision{Kind: TransitionNone, Reason: "engine_collection_owned"}
	}
	switch sub.Status {
	case string(models.StatusActive):
		if sub.PeriodEnd == nil || !sub.PeriodEnd.Before(now) {
			return Decision{Kind: TransitionNone}
		}
		if ev.Charge.RenewalPaymentAfterPeriodEnd {
			// Billing DID happen — the renewal/advance path owns the row.
			return Decision{Kind: TransitionNone, Reason: "renewal_payment_recorded"}
		}
		// No decline seen: the provider may have billed a renewal we have not
		// observed yet. Dunning here could charge the period twice; the
		// provider probe resolves the parked row.
		if now.Sub(*sub.PeriodEnd) > PeriodGrace {
			return Decision{Kind: TransitionParkUnknown, Reason: "no_decline_observed"}
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

// renewPaidPeriods renews one local period per verified charge between the
// cutoff and a later decline, from the local end. Without a known period
// length it is inconclusive.
func renewPaidPeriods(txns []RemoteTransaction, cutoff, declinedAt time.Time, start, end *time.Time, base Decision) Decision {
	if start == nil || end == nil || !end.After(*start) {
		base.Reason = "renewal_before_decline_cycle_unknown"
		return base
	}
	paid := 0
	for i := range txns {
		t := txns[i]
		if t.Type == TransactionTypeSale && t.Success && !t.OccurredAt.Before(cutoff) && t.OccurredAt.Before(declinedAt) {
			paid++
		}
	}
	newStart, newEnd := *end, end.Add(time.Duration(paid)*end.Sub(*start))
	return Decision{Kind: TransitionRenew, NewPeriodStart: &newStart, NewPeriodEnd: &newEnd, Reason: reasonRenewedBeforeDecline}
}

const reasonRenewedBeforeDecline = "verified_renewal_before_decline"

// paidPeriod is the period the verified charges since cutoff paid for: one
// local cycle per distinct charge, from the local end (audit 8). The
// provider's next billing date is adopted only when it lands on that many
// cycles (within half a cycle, which absorbs calendar months, or on that
// boundary's day); a date moved by a later decline is not payment. Without a known
// cycle only the provider's date can bound it.
func paidPeriod(txns []RemoteTransaction, cutoff time.Time, start, end *time.Time, remote *RemoteSubscription) (*time.Time, *time.Time) {
	next := remoteNextEnd(remote)
	var remoteStart *time.Time
	if remote != nil {
		remoteStart = remote.PeriodStart
	}
	if start == nil || end == nil || !end.After(*start) {
		if next == nil {
			return nil, nil
		}
		if end != nil {
			s := *end
			return &s, next
		}
		return remoteStart, next
	}
	seen := map[string]bool{}
	for _, t := range txns {
		if t.Type == TransactionTypeSale && t.Success && !t.OccurredAt.Before(cutoff) && !seen[t.TransactionID] {
			seen[t.TransactionID] = true
		}
	}
	cycle := end.Sub(*start)
	paid := time.Duration(max(len(seen), 1))
	from, to := *end, end.Add(paid*cycle)
	if next != nil && ((next.After(to.Add(-cycle/2)) && !next.After(to.Add(cycle/2))) || next.Equal(to.Truncate(24*time.Hour))) {
		to = *next // NMI states a date: the boundary's own day counts
	}
	return &from, &to
}

// ProviderDeclared names the snapshot a declared legacy import synthesizes.
const ProviderDeclared Provider = "declared"

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
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

// DecisionApplier applies decider transitions for the pull engine's enforce
// path. Fakeable for unit tests.
type DecisionApplier interface {
	ApplyDecision(ctx context.Context, subscriptionID uuid.UUID, d Decision) (bool, error)
}

// LifecycleDecisionApplier is the production DecisionApplier: load the row,
// route through ApplyDecision on the same merchant-scoped handle.
type LifecycleDecisionApplier struct {
	DB    *db.DB
	LC    *subscriptions.SubscriptionLifecycleService
	clock clockwork.Clock
}

// NewDecisionApplier builds the production applier. deferDelete may be nil
// (CLI pulls): a stale-decline cancel then logs the wiring gap instead of
// queuing the deferred NMI delete.
func NewDecisionApplier(database *db.DB, deferDelete subscriptions.DeferredDeleteScheduler, clocks ...clockwork.Clock) *LifecycleDecisionApplier {
	clock := timeutil.FirstClock(clocks...)
	lc := subscriptions.NewSubscriptionLifecycleService(database, nil, nil, nil, nil, nil, nil, clock)
	if deferDelete != nil {
		lc.SetDeferredDeleteScheduler(deferDelete)
	}
	return &LifecycleDecisionApplier{DB: database, LC: lc, clock: clock}
}

// SetClock keeps the decision instant and every lifecycle side effect on the
// owning reconcile engine's clock.
func (a *LifecycleDecisionApplier) SetClock(clock clockwork.Clock) {
	a.clock = timeutil.FirstClock(clock)
	a.LC.SetClock(a.clock)
}

func (a *LifecycleDecisionApplier) ApplyDecision(ctx context.Context, subscriptionID uuid.UUID, d Decision) (bool, error) {
	sub, err := subscriptions.NewSubscriptionRepo(a.DB).GetByID(ctx, subscriptionID)
	if err != nil {
		return false, fmt.Errorf("apply decision: load subscription %s: %w", subscriptionID, err)
	}
	return ApplyDecision(ctx, a.DB, a.LC, sub, d, a.clock.Now().UTC())
}

// stale reports whether the row moved since the decision was made on it.
func (d Decision) stale(sub *models.Subscription) bool {
	if d.DecidedStatus != "" && d.DecidedStatus != string(sub.Status) {
		return true
	}
	if d.DecidedPeriodEnd == nil {
		return false
	}
	return sub.CurrentPeriodEndsAt == nil || !sub.CurrentPeriodEndsAt.Equal(*d.DecidedPeriodEnd)
}
