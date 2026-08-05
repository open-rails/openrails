package converge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// pendingStaleAfter is how long a `pending` subscription may sit unconfirmed
// before life.subscription.pending_stale terminates it. Conservative: well beyond
// any real activation latency, so auto-cancelling can't race a confirming sub.
const pendingStaleAfter = 72 * time.Hour

// deriveBackfillWindow bounds derive-1 (#631): only subscriptions/payments whose
// access window ends within this lookback are derived. Banking-aligned 3y
// (replaces the migrate's old 1-year hack). A past-ended window is harmless
// (not live); the bound just keeps the merchant-wide sweep cheap.
// ponytail: const, not a config knob — make it a knob if a merchant needs to tune it.
const deriveBackfillWindow = 3 * 365 * 24 * time.Hour

// convergeScanCap bounds ONE pass's per-merchant detector scans (or#837). The
// sweep runs every 15 minutes across every armed merchant, so a scan whose size
// is the merchant's whole book is work scaling with records on file. Each capped
// scan is ordered by URGENCY (oldest lapse / longest-dead / oldest window), and
// a converge pass REMOVES what it repairs, so a merchant over the cap drains
// from the front across passes rather than losing findings. Deliberately far
// above any healthy merchant's drift: hitting it means something is wrong.
const convergeScanCap = 5000

// The three internal-plane passes, run in DERIVE → LIFE → CON order (sources must
// be truthful before grant effects are derived; lifecycle state must be current
// before final consistency checks). Each is fleshed out in #511 Phase D — the
// skeletons here emit nothing, so Converge is a verified no-op until the checks
// land. Every pass holds the engine for DB access + the grants layer (#514).

// derivePass — DERIVE plane: source → grant → grant effect. The
// heart of the engine; it drives derive-1/derive-2 (the #514 grants package, the
// sole writers of grants + grant effects) and verifies their output against the
// source ledger.
type derivePass struct{ e *ConvergeEngine }

func (*derivePass) Plane() string { return "DERIVE" }
func (p *derivePass) Run(ctx context.Context, scope Scope) ([]ConvergeFinding, error) {
	// DERIVE belongs to a customer (grants belong to a customer). A customer-scoped
	// Converge checks that one customer; the merchant-scoped sweep checks every
	// grant in ONE set query per check (#575 — `customer` nil), not one query per
	// grant-holder. Per-subscription scope has no extra DERIVE beyond its customer,
	// so it defers to the customer-scope run.
	if scope.Customer == nil && scope.Subscription != nil {
		return nil, nil // subscription-scope DERIVE rides on its customer-scope run
	}
	return p.runScope(ctx, scope, scope.Customer) // nil customer => merchant-wide sweep
}

// runScope runs the DERIVE checks for one customer (`customer` non-nil) or the
// whole merchant (`customer` nil — the sweep). Both paths funnel through the same
// set queries, so the checks can never diverge between inline and sweep.
func (p *derivePass) runScope(ctx context.Context, scope Scope, customer *uuid.UUID) ([]ConvergeFinding, error) {
	// derive.grant_effect.missing — every live grant has its derived effect
	// (entitlement windows / credit deposit). Repair = MaterializeGrant (#514,
	// idempotent). The remaining DERIVE checks (grant.*, grant_effect.mismatch)
	// follow.
	gl := grants.New(p.e.DB.Gen(ctx), scope.Merchant.UUID())
	gl.SetClock(p.e.Now)
	missing, err := gl.MissingEffects(ctx, customer)
	if err != nil {
		return nil, fmt.Errorf("derive: scan missing grant effects: %w", err)
	}
	out := make([]ConvergeFinding, 0, len(missing))
	for i := range missing {
		g := missing[i]
		out = append(out, ConvergeFinding{
			Type:       "derive.grant_effect.missing",
			Shape:      ShapeMissing,
			Class:      ClassAuto,
			Severity:   "high",
			SubjectKey: "grant_effect:" + g.ID.String(),
			Provider:   "self",
			Evidence:   map[string]any{"grant_id": g.ID.String(), "kind": g.Kind},
			Repair:     func(ctx context.Context) error { return p.lockedMaterialize(ctx, scope, g) },
		})
	}

	// derive.grant_effect.excess — a terminated grant whose effect was never
	// retracted (recorded revoke/expire that didn't propagate). Repair =
	// MaterializeGrant (retracts: entitlement revoke / credit clawback). The grant
	// + its termination are both present, so this is NOT the confirmed-absence
	// case (DomainNone) — it's AUTO propagation of a recorded decision. (The true
	// orphan case — a live effect with NO grant at all — is the gated/ADMIN variant,
	// added with merchant-wide enumeration.)
	unretracted, err := gl.UnretractedTerminations(ctx, customer)
	if err != nil {
		return nil, fmt.Errorf("derive: scan unretracted terminations: %w", err)
	}
	for i := range unretracted {
		g := unretracted[i]
		out = append(out, ConvergeFinding{
			Type:  "derive.grant_effect.excess",
			Shape: ShapeExcess,
			Class: ClassAuto,
			// Ungated: the justification is a LOCAL terminal fact — this grant
			// was already terminated — not an absence in provider data.
			Ungated:    true,
			Severity:   "high",
			SubjectKey: "grant_effect:" + g.ID.String(),
			Provider:   "self",
			Evidence:   map[string]any{"grant_id": g.ID.String(), "kind": g.Kind, "cause": "terminated_grant"},
			Repair:     func(ctx context.Context) error { return p.lockedMaterialize(ctx, scope, g) },
		})
	}

	// derive.grant.excess (grant tier) — a LIVE grant whose backing payment was
	// REFUNDED: the source no longer justifies the grant. Surface-only ADMIN, NOT
	// gated: the refund is a PRESENT recorded fact (not confirmed-absence), but a
	// refund that intentionally keeps access (goodwill) is legitimate, so an
	// operator decides whether to revoke — never auto-retracted. (The
	// subscription-cancelled case is handled at write time by the #511 revocation
	// unification, which terminates the grant when the effect is revoked, so it
	// does not surface here.)
	// derive.grant.missing (grant tier) — a completed, positive, one-off payment
	// for a product that PROMISES grants (non-empty entitlements/credits spec) yet
	// produced NO grant: "paid for a grantable product, got nothing" (a derive-1
	// failure). SPEC-AWARE — the product's own grant spec (payment→price→product)
	// is the positive signal, so empty-spec products / pure fees are never flagged.
	// MISSING → ADMIN surface-only: auto-granting re-runs derive-1 (product-spec-
	// dependent, owned by the purchase path), so an operator investigates the
	// creation failure and re-triggers rather than the engine guessing the window.
	// Severity CRITICAL (#690, Paul's ordering): this is the ORPHANED category —
	// taking money without delivering access — which outranks giving content
	// away (freeloader findings stay high).
	ungranted, err := gl.UngrantedGrantablePayments(ctx, customer)
	if err != nil {
		return nil, fmt.Errorf("derive: scan ungranted grantable payments: %w", err)
	}
	for i := range ungranted {
		p := ungranted[i]
		out = append(out, ConvergeFinding{
			Type:       "derive.grant.missing",
			Shape:      ShapeMissing,
			Class:      ClassAdmin,
			Severity:   "critical",
			SubjectKey: "payment:" + p.ID.String(),
			Provider:   "self",
			Evidence:   map[string]any{"payment_id": p.ID.String(), "amount": p.Amount, "currency": p.Currency, "cause": "grantable_payment_without_grant"},
			// surface-only: re-granting re-runs derive-1 (product-spec-dependent).
		})
	}

	// derive.subscription.missing (#631) — a stored subscription in an
	// access-granting state for a grantable product with NO grant. Unlike the
	// payment finding above this is AUTO-repaired: the window is unambiguous (the
	// subscription period), so the engine materializes grant + entitlement. NO-OP
	// for live subs (they already carry their grant); fires on the migrated cohort
	// after the migrate stops writing entitlements (#724).
	scanSince := p.e.Now().Add(-deriveBackfillWindow)
	ungrantedSubs, err := gl.UngrantedSubscriptions(ctx, customer, scanSince)
	if err != nil {
		return nil, fmt.Errorf("derive: scan ungranted subscriptions: %w", err)
	}
	for i := range ungrantedSubs {
		s := ungrantedSubs[i]
		out = append(out, ConvergeFinding{
			Type:       "derive.subscription.missing",
			Shape:      ShapeMissing,
			Class:      ClassAuto,
			Severity:   "high",
			SubjectKey: "subscription:" + s.ID.String(),
			Provider:   "self",
			Evidence:   map[string]any{"subscription_id": s.ID.String(), "status": string(s.Status), "cause": "subscription_without_grant"},
			Repair:     func(ctx context.Context) error { return gl.DeriveSubscriptionGrant(ctx, s) },
		})
	}

	// derive.wallet.missing (#631) — a completed solana wallet payment with a
	// stored access window and NO grant. AUTO-repaired (the stored expiration is the
	// unambiguous window). NO-OP for live; fires on the migrated wallet cohort.
	ungrantedWallet, err := gl.UngrantedWalletPayments(ctx, customer, scanSince)
	if err != nil {
		return nil, fmt.Errorf("derive: scan ungranted wallet payments: %w", err)
	}
	for i := range ungrantedWallet {
		w := ungrantedWallet[i]
		out = append(out, ConvergeFinding{
			Type:       "derive.wallet.missing",
			Shape:      ShapeMissing,
			Class:      ClassAuto,
			Severity:   "high",
			SubjectKey: "payment:" + w.ID.String(),
			Provider:   "self",
			Evidence:   map[string]any{"payment_id": w.ID.String(), "cause": "wallet_payment_without_grant"},
			Repair:     func(ctx context.Context) error { return gl.DeriveWalletGrant(ctx, w) },
		})
	}

	refunded, err := gl.RefundedSourceGrants(ctx, customer)
	if err != nil {
		return nil, fmt.Errorf("derive: scan grants with refunded source: %w", err)
	}
	for i := range refunded {
		g := refunded[i]
		pid := ""
		if g.PaymentID != nil {
			pid = g.PaymentID.String()
		}
		out = append(out, ConvergeFinding{
			Type:       "derive.grant.excess",
			Shape:      ShapeExcess,
			Class:      ClassAdmin,
			Severity:   "high",
			SubjectKey: "grant:" + g.ID.String(),
			Provider:   "self",
			Evidence:   map[string]any{"grant_id": g.ID.String(), "kind": g.Kind, "payment_id": pid, "cause": "refunded_payment"},
			// surface-only: revoking access on a refund is an operator decision.
		})
	}

	// derive.grant_effect.mismatch (#665, moved from the legacy pull engine's
	// PS-9) — subscription ↔ entitlement-projection drift, both directions.
	// Grant direction: an `active` sub in a RUNNING period whose product
	// promises features that were NEVER projected for this period (revoked
	// windows are recorded decisions and count as projected, §6; no-grant subs
	// belong to derive.subscription.missing). Repair = derive-1 for exactly the
	// missing features — grants stay the sole effect writer.
	q := p.e.DB.Gen(ctx)
	now := p.e.Now()
	unprojected, err := q.ListActiveSubsMissingEntitlementProjection(ctx, gen.ListActiveSubsMissingEntitlementProjectionParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: customer, Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("derive: scan unprojected subscriptions: %w", err)
	}
	for i := range unprojected {
		s := unprojected[i]
		out = append(out, ConvergeFinding{
			Type:       "derive.grant_effect.mismatch",
			Shape:      ShapeMismatch,
			Class:      ClassAuto,
			Severity:   "high",
			SubjectKey: "subscription:" + s.ID.String(),
			Provider:   "self",
			Evidence: map[string]any{
				"subscription_id": s.ID.String(), "customer_id": s.CustomerID.String(),
				"direction": "grant", "missing_features": json.RawMessage(s.EntitlementsSpec),
			},
			Repair: func(ctx context.Context) error {
				// Row shape matches ListUngrantedSubscriptions; EntitlementsSpec
				// carries ONLY the missing features, so derive-1 fills the gap.
				return gl.DeriveSubscriptionGrant(ctx, gen.ListUngrantedSubscriptionsRow(s))
			},
		})
	}

	// Revoke direction: a terminally-dead sub (cancelled/expired/failed —
	// `unknown` keeps access, #664) still projecting subscription-sourced
	// BOUNDED windows past its entitled bound. #690/#691 paid-through guard:
	// the bound is GREATEST(paid-through, ended_at) — a user cancel leaves a
	// PAID RUNWAY window bounded to period end, which is never excess before
	// the bound. Propagation of a recorded terminal decision, so AUTO and NOT
	// confirmed-absence gated; repair writes the missed/correct #691 closure
	// (bounds live windows at the bound, drops scheduled ones past it) so the
	// runway survives while the overrun is cleaned. STANDING (end_at NULL)
	// windows of terminal subs are the freeloader case (derive.entitlement.
	// unjustified below, ADMIN) — the partition keeps one condition per check.
	dead, err := q.ListDeadSubsWithLiveEntitlements(ctx, gen.ListDeadSubsWithLiveEntitlementsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: customer, Now: now, RowLimit: convergeScanCap,
	})
	if err != nil {
		return nil, fmt.Errorf("derive: scan dead subscriptions with live entitlements: %w", err)
	}
	for i := range dead {
		d := dead[i]
		closeAt := now // no recorded bound: close at detection time
		evidence := map[string]any{
			"subscription_id": d.ID.String(), "customer_id": d.CustomerID.String(),
			"status": string(d.Status), "direction": "revoke",
		}
		if bound := latestTime(d.CurrentPeriodEndsAt, d.EndedAt); bound != nil {
			closeAt = bound.UTC()
			evidence["entitled_bound"] = closeAt
		}
		out = append(out, ConvergeFinding{
			Type:       "derive.grant_effect.mismatch",
			Shape:      ShapeMismatch,
			Class:      ClassAuto,
			Severity:   "high",
			SubjectKey: "subscription:" + d.ID.String(),
			Provider:   "self",
			Evidence:   evidence,
			Repair: func(ctx context.Context) error {
				ctx = merchant.WithID(ctx, scope.Merchant)
				return entitlements.NewEntitlementService(p.e.DB).BoundSubscriptionAccess(ctx, d.ID, closeAt)
			},
		})
	}

	// derive.entitlement.unjustified (#690, renamed from derive.entitlement.
	// orphan in migration 066 — "orphaned" is the paying-without-access
	// category) — the FREELOADER detector: a LIVE window whose source is
	// PROVEN dead or absent (sub row missing; standing window of a terminal
	// sub whose closure never landed; refunded one-off payment) with no live
	// grant justifying the access. Post-#691 fail-open, stale is NOT
	// freeloading — standing windows of live/unknown/past_due subs never
	// surface here. ADMIN surface-only (policy: access removal is an operator
	// decision, never automatic on a derived conclusion), with the #692
	// revoke/admin-grant recommendation pair.
	unjustified, err := q.ListUnjustifiedEntitlementWindows(ctx, gen.ListUnjustifiedEntitlementWindowsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: customer, Now: now, RowLimit: convergeScanCap,
	})
	if err != nil {
		return nil, fmt.Errorf("derive: scan unjustified entitlement windows: %w", err)
	}
	for i := range unjustified {
		out = append(out, unjustifiedEntitlementFinding(&unjustified[i]))
	}
	return out, nil
}

// latestTime returns the later of two nullable instants (nil when both nil).
func latestTime(a, b *time.Time) *time.Time {
	if a == nil {
		return b
	}
	if b == nil || a.After(*b) {
		return a
	}
	return b
}

// unjustifiedEntitlementFinding renders one freeloader window as an ADMIN
// finding carrying the #692 recommendation: revoke (default; as-of the
// entitled bound when the terminal sub recorded one) or record an admin grant
// instead.
func unjustifiedEntitlementFinding(o *gen.ListUnjustifiedEntitlementWindowsRow) ConvergeFinding {
	asOf := ""
	bound := latestTime(o.SubPeriodEndsAt, o.SubEndedAt)
	if o.Cause == "terminal_subscription_standing" && bound != nil {
		asOf = bound.UTC().Format(time.RFC3339)
	}
	productID := ""
	if o.SubProductID != nil {
		productID = o.SubProductID.String()
	}
	if o.PaymentProductID != nil {
		productID = o.PaymentProductID.String()
	}
	alt := recommend.RecordAdminGrantRec(o.CustomerID.String(), productID, "known-legitimate access")
	rec := recommend.RevokeEntitlementRec(o.EntitlementID.String(), asOf, &alt)

	ev := map[string]any{
		"entitlement_id": o.EntitlementID.String(), "customer_id": o.CustomerID.String(),
		"entitlement": o.Entitlement, "source_type": o.SourceType, "source_id": o.SourceID.String(),
		"cause":               o.Cause,
		recommend.EvidenceKey: rec.Map(),
	}
	if o.SubStatus != nil && *o.SubStatus != "" {
		ev["subscription_status"] = *o.SubStatus
	}
	if bound != nil {
		ev["entitled_bound"] = bound.UTC()
	}
	if o.PaymentID != nil {
		ev["payment_id"] = o.PaymentID.String()
	}

	var prose string
	switch o.Cause {
	case "missing_subscription":
		prose = fmt.Sprintf("Live entitlement %q for customer %s references subscription %s, which does not exist — access has no justification. Revoke the window, or record an admin grant if it is known-legitimate.",
			o.Entitlement, o.CustomerID, o.SourceID)
	case "terminal_subscription_standing":
		prose = fmt.Sprintf("Standing (open-ended) entitlement %q outlives its terminal subscription %s and its paid runway — the closure event never bounded it. Revoke the window as of the entitled bound, or record an admin grant if it is known-legitimate.",
			o.Entitlement, o.SourceID)
	default: // refunded_payment
		prose = fmt.Sprintf("Live entitlement %q is sourced by refunded payment %s with no live grant justifying the access. Revoke the window, or record an admin grant if it is known-legitimate.",
			o.Entitlement, o.SourceID)
	}

	return ConvergeFinding{
		Type:              "derive.entitlement.unjustified",
		Shape:             ShapeExcess,
		Class:             ClassAdmin,
		Severity:          "high",
		SubjectKey:        "entitlement:" + o.EntitlementID.String(),
		Provider:          "self",
		Evidence:          ev,
		RecommendedAction: prose,
		// surface-only (policy, #690): never auto-revoke on a derived conclusion.
	}
}

// lockedMaterialize runs a MaterializeGrant repair inside a merchant tx under
// the per-customer spend lock (#677): the credit legs (deposit / clawback) are
// check-then-write, so overlapping converge runs must serialize with each
// other and with spends. The entitlement/ownership legs are lock-cheap no-ops
// when already projected.
func (p *derivePass) lockedMaterialize(ctx context.Context, scope Scope, g gen.OpenrailsGrant) error {
	ctx = merchant.WithID(ctx, scope.Merchant)
	return p.e.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		gl := grants.New(gen.New(tx), scope.Merchant.UUID())
		gl.SetClock(p.e.Now)
		if err := gl.LockCustomer(ctx, g.CustomerID); err != nil {
			return err
		}
		return gl.MaterializeGrant(ctx, g)
	})
}

// lifePass — LIFE plane: clock + state machine. Converge-not-replay:
// it computes where a record should be NOW and moves it there, never re-running
// skipped side effects.
type lifePass struct{ e *ConvergeEngine }

func (*lifePass) Plane() string { return "LIFE" }
func (p *lifePass) Run(ctx context.Context, scope Scope) ([]ConvergeFinding, error) {
	// life.checkout_session.stale — an expired, non-terminal checkout session is
	// cleaned up. EXCESS but time-driven (NOT confirmed-absence gated), so AUTO.
	// Subscription lifecycle checks (period/dunning/grace/pending) + provider-intent
	// staleness follow.
	q := p.e.DB.Gen(ctx)
	now := p.e.Now()
	stale, err := q.ListStaleCheckoutSessions(ctx, gen.ListStaleCheckoutSessionsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: scope.Customer, Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("life: scan stale checkout sessions: %w", err)
	}
	out := make([]ConvergeFinding, 0, len(stale))
	for i := range stale {
		id := stale[i]
		out = append(out, ConvergeFinding{
			Type:  "life.checkout_session.stale",
			Shape: ShapeExcess,
			Class: ClassAuto,
			// Ungated: expiring an abandoned checkout session retracts no
			// access, no money and no provider object.
			Ungated:    true,
			Severity:   "low",
			SubjectKey: "checkout_session:" + id.String(),
			Provider:   "self",
			Evidence:   map[string]any{"checkout_session_id": id.String()},
			Repair: func(ctx context.Context) error {
				_, e := q.ExpireCheckoutSessionByID(ctx, gen.ExpireCheckoutSessionByIDParams{
					MerchantID: scope.Merchant.UUID(), ID: id, Now: p.e.Now(),
				})
				return e
			},
		})
	}

	// The lapsed cohort (#664/#665) — ONE scan (active past period end, or
	// past_due with grace elapsed and no retry scheduled) carrying its evidence
	// legs; the ONE decider (reconcile.Decide) chooses the transition and the
	// repair applies it through the shared lifecycle (reconcile.ApplyDecision).
	// LIFE carries no provider snapshot, so the decider can only enter dunning
	// (ownership evidence) or PARK as `unknown` (access intact) — convergence
	// never terminally cancels; FailMembership + provider-confirmed outcomes
	// own that. #691: grace here is a PACING marker only — an auto-renew sub's
	// entitlement window is STANDING, so none of these transitions touch
	// access. Finding vocabulary is unchanged:
	//   active + ownership evidence  → life.subscription.period_overdue
	//   active, no evidence          → life.subscription.needs_verification
	//   past_due, dunning stalled    → life.subscription.grace_exhausted
	lapsed, err := q.ListLapsedSubscriptionsWithEvidence(ctx, gen.ListLapsedSubscriptionsWithEvidenceParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: scope.Customer, Now: now, RowLimit: convergeScanCap,
	})
	if err != nil {
		return nil, fmt.Errorf("life: scan lapsed subscriptions: %w", err)
	}
	// #835 evidence-staleness floor. LIFE cannot reach a cancel today (its
	// certainty legs are unpopulated), but the floor travels with the decider
	// invocation so the day a plane starts producing them it is already gated.
	floor := reconcile.EvidenceFloorFor(ctx, p.e.DB, scope.Merchant.UUID())
	for i := range lapsed {
		row := lapsed[i]
		state := reconcile.SubscriptionState{
			Status:             string(row.Status),
			Rail:               row.Rail,
			HasPaymentMethod:   row.HasPaymentMethod,
			RailSubscriptionID: row.RailSubscriptionID,
			PeriodEnd:          row.CurrentPeriodEndsAt,
			GraceEndsAt:        row.GraceEndsAt,
			NextRetryScheduled: row.NextRetryAt != nil,
		}
		d := reconcile.Decide(state, reconcile.EvidenceBundle{
			Charge: reconcile.ChargeEvidence{
				PaymentOpenedCurrentPeriod:   row.PaymentOpenedPeriod,
				RenewalPaymentAfterPeriodEnd: row.RenewalPaymentAfterEnd,
			},
			WatermarkNewerThanPeriodEnd: row.WatermarkNewerThanPeriodEnd,
			EvidenceFloor:               floor,
		}, now, 0)

		var ftype, severity string
		evidence := map[string]any{"subscription_id": row.ID.String(), "cause": d.Reason}
		switch d.Kind {
		case reconcile.TransitionPastDue:
			ftype, severity = "life.subscription.period_overdue", "medium"
			evidence["grace_ends_at"] = d.GraceEndsAt
		case reconcile.TransitionParkUnknown:
			if row.Status == gen.OpenrailsSubscriptionStatus(models.StatusPastDue) {
				ftype, severity = "life.subscription.grace_exhausted", "high"
				if row.GraceEndsAt != nil {
					evidence["grace_ends_at"] = *row.GraceEndsAt
				}
			} else {
				ftype, severity = "life.subscription.needs_verification", "medium"
				evidence["rail"] = row.Rail
			}
		default:
			continue // no evidence-justified move (e.g. within grace slack)
		}
		subID, decision := row.ID, d
		out = append(out, ConvergeFinding{
			Type:       ftype,
			Shape:      ShapeMismatch,
			Class:      ClassAuto,
			Severity:   Severity(severity),
			SubjectKey: "subscription:" + subID.String(),
			Provider:   "self",
			Evidence:   evidence,
			Repair: func(ctx context.Context) error {
				sub, err := subscriptions.NewSubscriptionRepo(p.e.DB).GetByID(ctx, subID)
				if err != nil {
					return fmt.Errorf("life: load lapsed subscription %s: %w", subID, err)
				}
				_, err = reconcile.ApplyDecision(ctx, p.e.DB, p.e.lifecycle, sub, decision, now)
				return err
			},
		})
	}

	// life.subscription.pending_stale — a `pending` sub that never confirmed
	// within the threshold is abandoned; cancel it (no entitlements/money to
	// unwind). EXCESS → gated on the `subscriptions` domain (#842): the repair
	// is justified by an absence (no confirmation arrived), and an absence is
	// only proof once provider truth has been fully reconciled.
	stalePending, err := q.ListStalePendingSubscriptions(ctx, gen.ListStalePendingSubscriptionsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: scope.Customer, Cutoff: now.Add(-pendingStaleAfter),
		RowLimit: convergeScanCap,
	})
	if err != nil {
		return nil, fmt.Errorf("life: scan stale pending subscriptions: %w", err)
	}
	for i := range stalePending {
		subID := stalePending[i]
		out = append(out, ConvergeFinding{
			Type:  "life.subscription.pending_stale",
			Shape: ShapeExcess,
			Class: ClassAuto,
			// #842: this cancels a subscription because a confirmation did not
			// ARRIVE — an absence, and absence needs the §3.2 proof. A delayed
			// or dropped webhook is not evidence the customer did not pay.
			SourceDomain: DomainSubscriptions,
			Severity:     "low",
			SubjectKey:   "subscription:" + subID.String(),
			Provider:     "self",
			Evidence:     map[string]any{"subscription_id": subID.String()},
			Repair: func(ctx context.Context) error {
				// Terminal cancel through the shared core. A never-confirmed pending
				// sub has no entitlements/money to unwind (RevokeSources empty); the
				// Solana cascade is a tolerant no-op when no row was ever enrolled.
				sub, err := subscriptions.NewSubscriptionRepo(p.e.DB).GetByID(ctx, subID)
				if err != nil {
					return fmt.Errorf("life: load stale-pending subscription %s: %w", subID, err)
				}
				fb := "pending stale (never confirmed)"
				return p.e.lifecycle.ApplyLocalCancellation(ctx, p.e.DB, sub, subscriptions.LocalCancellation{
					EndedAt:    now,
					CancelType: models.CancelTypeExpired,
					Feedback:   &fb,
				})
			},
		})
	}

	// life.subscription.dunning_overdue — a past_due sub still within its grace
	// window but with NO retry scheduled: its dunning schedule stalled (a missed
	// enqueue, a crash between attempts). MISSING → materialize the schedule so
	// the dunning worker resumes. Converge-not-replay: we schedule the NEXT retry
	// (now), we do NOT re-run the missed attempts; a sub whose grace has actually
	// elapsed is handled by grace_exhausted, not here. AUTO, not gated.
	dunningStalled, err := q.ListDunningStalledSubscriptions(ctx, gen.ListDunningStalledSubscriptionsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: scope.Customer, Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("life: scan dunning-stalled subscriptions: %w", err)
	}
	for i := range dunningStalled {
		subID := dunningStalled[i]
		out = append(out, ConvergeFinding{
			Type:       "life.subscription.dunning_overdue",
			Shape:      ShapeMissing,
			Class:      ClassAuto,
			Severity:   "medium",
			SubjectKey: "subscription:" + subID.String(),
			Provider:   "self",
			Evidence:   map[string]any{"subscription_id": subID.String(), "next_retry_at": now},
			Repair: func(ctx context.Context) error {
				_, e := q.SetSubscriptionNextRetry(ctx, gen.SetSubscriptionNextRetryParams{
					ID: subID, NextRetryAt: p.e.Now(),
				})
				return e
			},
		})
	}

	// life.provider_intent.abandoned — a desired provider action that will not
	// auto-retry (terminal/expired or past deadline) needs a human. Surface-only:
	// MISMATCH → ADMIN, no auto-repair (the resolution is an operator/provider
	// action). rail_intents are merchant-level (no customer_id), so this runs
	// at merchant/subscription scope, not per-customer inline.
	if scope.Customer == nil || scope.Subscription != nil {
		abandoned, err := q.ListAbandonedProviderIntents(ctx, gen.ListAbandonedProviderIntentsParams{
			MerchantID: scope.Merchant.UUID(), SubscriptionID: scope.Subscription, Now: now,
		})
		if err != nil {
			return nil, fmt.Errorf("life: scan abandoned provider intents: %w", err)
		}
		for i := range abandoned {
			pi := abandoned[i]
			out = append(out, ConvergeFinding{
				Type:       "life.provider_intent.abandoned",
				Shape:      ShapeMismatch,
				Class:      ClassAdmin,
				Severity:   "high",
				SubjectKey: "provider_intent:" + pi.ID.String(),
				Provider:   "self",
				Evidence:   map[string]any{"provider_intent_id": pi.ID.String(), "intent_type": pi.IntentType, "intent_status": pi.Status, "provider": pi.Rail},
				// surface-only: no Repair (operator/provider action resolves it)
			})
		}
	}

	// life.provider_intent.stuck (#665, moved from the legacy pull engine's
	// PS-10) — a rail intent sitting non-terminal beyond the stuck thresholds.
	// LOCAL ledger only, merchant-wide (intents carry no customer), so it runs
	// on the sweep/post-pull scope. Mode/kill-switch parks are informational
	// (the executor drains them when the blocker lifts); everything else means
	// provider failures, bad credentials, or a dead executor/verifier. NEVER
	// repaired here: the intent executor/verifier own the intent.
	if scope.IsGlobal() {
		actionCutoff, verifyCutoff := now.Add(-stuckActionableAge), now.Add(-stuckVerifyAge)
		stuck, err := q.ListStuckRailIntents(ctx, gen.ListStuckRailIntentsParams{
			ActionCutoff: actionCutoff, VerifyCutoff: verifyCutoff,
		})
		if err != nil {
			return nil, fmt.Errorf("life: scan stuck rail intents: %w", err)
		}
		for i := range stuck {
			out = append(out, stuckIntentFinding(&stuck[i], now))
		}
		// Recovered intents resolve subject-first (converge has no run-driven
		// vanish sweep): open stuck findings whose intent no longer meets the
		// stuck criteria auto-resolve.
		if _, err := q.AutoResolveRecoveredStuckIntentFindings(ctx, gen.AutoResolveRecoveredStuckIntentFindingsParams{
			MerchantID: scope.Merchant.UUID(), ActionCutoff: actionCutoff, VerifyCutoff: verifyCutoff,
		}); err != nil {
			return nil, fmt.Errorf("life: resolve recovered stuck-intent findings: %w", err)
		}
	}
	return out, nil
}

// Stuck-intent thresholds — HARDCODED (no-knobs policy): the executor runs
// every minute, so a day-old actionable intent means provider failures, bad
// credentials, a dead worker — or a deliberate park; a healthy verifier
// resolves unknowns in minutes, and an in_flight lease outliving hours means
// a dead executor.
const (
	stuckActionableAge = 24 * time.Hour // pending / failed_retryable
	stuckVerifyAge     = 2 * time.Hour  // in_flight / unknown_needs_verify
)

// isModeParkedReason reports whether last_failure_reason records a deliberate
// park by the operating mode / kill switch (intents.GateExecution "mode=..."
// strings). Parked is not broken — the executor drains the queue when the
// blocker lifts — so the finding is informational, not the admin queue.
func isModeParkedReason(reason string) bool { return strings.Contains(reason, "mode=") }

// stuckIntentFinding diagnoses one stuck rail intent. SubjectKey is the BARE
// intent id — ledger continuity with the legacy pull-engine emissions of the
// same finding type. Provider is the intent's own rail.
func stuckIntentFinding(si *gen.OpenrailsRailIntent, now time.Time) ConvergeFinding {
	age := now.Sub(si.CreatedAt)
	ev := map[string]any{
		"intent_id":       si.ID.String(),
		"intent_type":     si.IntentType,
		"provider":        si.Rail,
		"origin":          si.Origin,
		"status":          si.Status,
		"attempts":        si.Attempts,
		"created_at":      si.CreatedAt.Format(time.RFC3339),
		"next_attempt_at": si.NextAttemptAt.Format(time.RFC3339),
		"age":             age.Truncate(time.Minute).String(),
	}
	if si.SubscriptionID != nil {
		ev["subscription_id"] = si.SubscriptionID.String()
	}
	if si.PaymentID != nil {
		ev["payment_id"] = si.PaymentID.String()
	}
	if si.OriginReason != nil && *si.OriginReason != "" {
		ev["origin_reason"] = *si.OriginReason
	}
	if si.LastFailureReason != nil && *si.LastFailureReason != "" {
		ev["last_failure_reason"] = *si.LastFailureReason
	}
	if si.ExpiresAt != nil {
		ev["expires_at"] = si.ExpiresAt.Format(time.RFC3339)
	}
	f := ConvergeFinding{
		Type:       "life.provider_intent.stuck",
		Shape:      ShapeMismatch,
		Class:      ClassAdmin, // requires_review: needs a human
		Severity:   "high",
		SubjectKey: si.ID.String(),
		Provider:   si.Rail,
		Evidence:   ev,
		// surface-only: check and converge never touch the intent itself.
	}
	if si.LastFailureReason != nil && isModeParkedReason(*si.LastFailureReason) {
		f.Class = ClassAuto // no Repair → reconcile_required (informational)
		f.Severity = "low"
	}
	return f
}

// accessEndedLookback bounds the NOTIFY access-ended scan: a window closed
// longer ago than this is stale news, never emailed.
const accessEndedLookback = 30 * 24 * time.Hour

// notifyPass — NOTIFY plane (#789): whatever plane closed a customer's LAST
// entitlement window (dunning, reconcile-driven cancel, grant lapse), the
// customer is told exactly once. The pass only CREATES notification_queue rows
// (emailed_at NULL); delivery belongs to the notification email sweep — the
// converge engine carries no EmailService. Dedupe: any premium_ended row at or
// after the close means a transition site already told them.
type notifyPass struct{ e *ConvergeEngine }

func (*notifyPass) Plane() string { return "NOTIFY" }
func (p *notifyPass) Run(ctx context.Context, scope Scope) ([]ConvergeFinding, error) {
	if scope.Customer == nil && scope.Subscription != nil {
		return nil, nil // subscription-scope rides on its customer/merchant run
	}
	q := p.e.DB.Gen(ctx)
	now := p.e.Now()
	closed, err := q.ListRecentlyClosedLastEntitlementWindows(ctx, gen.ListRecentlyClosedLastEntitlementWindowsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: scope.Customer,
		ClosedAfter: now.Add(-accessEndedLookback), Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("notify: scan recently closed entitlement windows: %w", err)
	}
	var out []ConvergeFinding
	repo := subscriptions.NewNotificationQueueRepo(p.e.DB)
	for i := range closed {
		c := closed[i]
		if c.ClosedAt == nil {
			continue // WHERE guarantees a bound; guard the pointer anyway
		}
		closedAt := c.ClosedAt.UTC()
		already, err := q.PremiumEndedNotificationExistsSince(ctx, gen.PremiumEndedNotificationExistsSinceParams{
			MerchantID: scope.Merchant.UUID(), CustomerID: c.CustomerID, Since: closedAt,
		})
		if err != nil {
			return nil, fmt.Errorf("notify: check premium_ended dedupe: %w", err)
		}
		if already {
			continue
		}
		cust, entName := c.CustomerID, c.Entitlement
		out = append(out, ConvergeFinding{
			Type:       "notify.access_ended.missing",
			Shape:      ShapeMissing,
			Class:      ClassAuto,
			Severity:   "low",
			SubjectKey: "customer:" + cust.String(),
			Provider:   "self",
			Evidence: map[string]any{
				"customer_id": cust.String(), "entitlement": entName,
				"ended_at": closedAt.Format(time.RFC3339), "source_type": c.SourceType, "source_id": c.SourceID.String(),
			},
			Repair: func(ctx context.Context) error {
				ctx = merchant.WithID(ctx, scope.Merchant)
				return repo.Create(ctx, &models.NotificationQueue{
					ID:         uuidutil.NewV7(),
					CustomerID: cust,
					EventType:  models.NotificationPremiumEnded,
					Data: map[string]any{
						"reason":      string(subscriptions.PremiumEndReasonAccessEnded),
						"ended_at":    closedAt.Format(time.RFC3339),
						"entitlement": entName,
						"source":      "converge_notify",
					},
				})
			},
		})
	}
	return out, nil
}

// conPass — CON plane: residual internal consistency (duplicate /
// amount_mismatch / reference).
type conPass struct{ e *ConvergeEngine }

func (*conPass) Plane() string { return "CON" }
func (p *conPass) Run(ctx context.Context, scope Scope) ([]ConvergeFinding, error) {
	// CON is intentionally small (spec §CON): the residual internal-accounting /
	// referential checks that aren't already a DB constraint, a LIFE state-machine
	// transition, or a DERIVE grant effect. Findings here are surface-only — a
	// dangling reference or a duplicate has no safe automatic repair, it needs an
	// admin/operator decision — so they are ADMIN, no Repair closure.
	//
	// consistency.reference.source_reference — an entitlement's polymorphic
	// source_type/source_id pair resolves to no row (or, under the merchant GUC, a
	// row in the wrong merchant). EXCESS/MISMATCH → ADMIN. Customer-scope filters
	// in code (the underlying audit queries are merchant-wide via RLS); merchant-
	// scope reports all. The remaining CON subtypes (duplicate.*, amount_mismatch.*)
	// layer onto this same harness.
	q := p.e.DB.Gen(ctx)
	now := p.e.Now()
	var out []ConvergeFinding

	// Customer-scoped when scope.Customer is set (inline Converge(customer) → the
	// scan is O(that customer)), merchant-wide when nil (the sweep). The SQL does
	// the filtering, so an after-every-mutation invocation stays cheap.
	// #690 partition: LIVE windows with dangling sub sources are freeloaders
	// (derive.entitlement.unjustified); this reference check keeps the
	// non-live rest.
	cust := scope.Customer
	orphanSubs, err := q.ConOrphanEntitlementSubscriptionSource(ctx, gen.ConOrphanEntitlementSubscriptionSourceParams{
		Now: now, CustomerID: cust,
	})
	if err != nil {
		return nil, fmt.Errorf("con: scan orphan subscription sources: %w", err)
	}
	orphanPays, err := q.ConOrphanEntitlementPaymentSource(ctx, cust)
	if err != nil {
		return nil, fmt.Errorf("con: scan orphan payment sources: %w", err)
	}
	// (No admin-source orphan check: #511 retired entitlement_grants — manually
	// granted entitlements are now `admin`-sourced grants in the ledger, covered
	// by DERIVE via grant_id, with no separate provenance row to dangle.)

	emit := func(entID uuid.UUID, userID, entitlement, sourceType string, sourceID uuid.UUID) {
		out = append(out, ConvergeFinding{
			Type:       "consistency.reference.source_reference",
			Shape:      ShapeMismatch,
			Class:      ClassAdmin,
			Severity:   "medium",
			SubjectKey: "entitlement:" + entID.String(),
			Provider:   "self",
			Evidence: map[string]any{
				"entitlement_id": entID.String(), "customer_id": userID,
				"entitlement": entitlement, "source_type": sourceType, "source_id": sourceID.String(),
			},
			// surface-only: a dangling source has no safe auto-repair (it may be a
			// valid historical record or a real corruption) — an admin decides.
		})
	}
	for i := range orphanSubs {
		r := orphanSubs[i]
		emit(r.EntID, r.UserID, r.Entitlement, r.SourceType, r.SourceID)
	}
	for i := range orphanPays {
		r := orphanPays[i]
		emit(r.EntID, r.UserID, r.Entitlement, r.SourceType, r.SourceID)
	}

	// consistency.duplicate.provider_charge — more than one settled, non-refunded
	// charge for the same customer/product/month where only one is expected and no
	// invoice/proration/operator action explains the overlap (folded from the
	// retired audit D-2 check). EXCESS → ADMIN, surface-only: collecting money
	// twice is never auto-undone (a refund is an operator decision); the finding
	// carries the duplicate payment ids + the #692 refund recommendation.
	// Severity CRITICAL (#690, Paul): a duplicate charge is money harm — worse
	// than a freeloader's marginal content access.
	dupCharges, err := q.ConDuplicateChargesSamePeriod(ctx, cust)
	if err != nil {
		return nil, fmt.Errorf("con: scan duplicate charges: %w", err)
	}
	for i := range dupCharges {
		d := dupCharges[i]
		ids := make([]string, len(d.PaymentIds))
		for j, id := range d.PaymentIds {
			ids[j] = id.String()
		}
		// payment_ids is ordered purchased_at DESC: ids[0] is the later charge —
		// the default refund target (operator can override before approving).
		rec := recommend.CancelAndRefundRec("", ids[0])
		out = append(out, ConvergeFinding{
			Type:       "consistency.duplicate.provider_charge",
			Shape:      ShapeExcess,
			Class:      ClassAdmin,
			Severity:   "critical",
			SubjectKey: "provider_charge:" + d.UserID + ":" + d.ProductID.String() + ":" + d.FirstDate.Format("2006-01"),
			Provider:   "self",
			Evidence: map[string]any{
				"customer_id": d.UserID, "product_id": d.ProductID.String(), "product_key": d.ProductKey,
				"charge_count": d.Count, "payment_ids": ids, "total_amount": d.TotalAmount,
				"first_date": d.FirstDate, "last_date": d.LastDate,
				recommend.EvidenceKey: rec.Map(),
			},
			RecommendedAction: fmt.Sprintf("Customer %s was charged %d times for product %q in %s (total %d micros; payments %s). Refund the later charge %s unless an invoice/proration/operator action explains the overlap.",
				d.UserID, d.Count, d.ProductKey, d.FirstDate.Format("2006-01"), d.TotalAmount, strings.Join(ids, ", "), ids[0]),
			// surface-only: a refund/credit is an operator decision, never automatic.
		})
	}

	// consistency.duplicate.ownership (#690) — more than one LIVE paid
	// ownership grant per (customer, product): the cross-month one-off/
	// lifetime double-purchase that duplicate.provider_charge's month scope
	// misses. CRITICAL (customer charged twice), ADMIN surface-only, with the
	// #692 cancel_and_refund recommendation targeting the LATER purchase
	// (default only — override_params can flip it before approving).
	dupOwn, err := q.ConDuplicateOwnershipGrants(ctx, gen.ConDuplicateOwnershipGrantsParams{Now: now, CustomerID: cust})
	if err != nil {
		return nil, fmt.Errorf("con: scan duplicate ownership grants: %w", err)
	}
	for i := range dupOwn {
		f, err := duplicateOwnershipFinding(&dupOwn[i])
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// ownershipPurchase is one leg of a duplicate-ownership group (decoded from
// the query's jsonb purchases array, ordered oldest-first).
type ownershipPurchase struct {
	GrantID     string  `json:"grant_id"`
	SourceType  string  `json:"source_type"`
	SourceID    string  `json:"source_id"`
	PaymentID   *string `json:"payment_id"`
	Amount      *int64  `json:"amount"`
	Currency    *string `json:"currency"`
	PurchasedAt string  `json:"purchased_at"`
}

func (p ownershipPurchase) describe() string {
	s := "grant " + p.GrantID + " (purchased " + p.PurchasedAt
	if p.PaymentID != nil {
		s += ", payment " + *p.PaymentID
	}
	if p.Amount != nil && p.Currency != nil {
		s += fmt.Sprintf(", %d micros %s", *p.Amount, *p.Currency)
	}
	if p.SourceType == "subscription" {
		s += ", subscription " + p.SourceID
	}
	return s + ")"
}

// duplicateOwnershipFinding renders one duplicate-ownership group. The
// recommendation cancels the later purchase's subscription (when it is
// subscription-shaped) and refunds its payment; a pure one-off duplicate
// carries refund_payment_id only. No payment linkage at all → prose only
// (approve unavailable; the operator resolves out-of-band).
func duplicateOwnershipFinding(d *gen.ConDuplicateOwnershipGrantsRow) (ConvergeFinding, error) {
	var purchases []ownershipPurchase
	if err := json.Unmarshal(d.Purchases, &purchases); err != nil {
		return ConvergeFinding{}, fmt.Errorf("con: decode duplicate ownership purchases: %w", err)
	}
	later := purchases[len(purchases)-1]
	subID, refundPay := "", ""
	if later.SourceType == "subscription" {
		subID = later.SourceID
	}
	if later.PaymentID != nil {
		refundPay = *later.PaymentID
	}
	productID := ""
	if d.ProductID != nil {
		productID = d.ProductID.String()
	}

	descs := make([]string, len(purchases))
	for i := range purchases {
		descs[i] = purchases[i].describe()
	}
	prose := fmt.Sprintf("Customer %s holds %d live ownership grants for product %q — charged more than once for the same product: %s. Cancel/refund the later purchase (default) unless the earlier one is the mistake.",
		d.CustomerID, d.Count, d.ProductKey, strings.Join(descs, "; "))

	ev := map[string]any{
		"customer_id": d.CustomerID.String(), "product_id": productID, "product_key": d.ProductKey,
		"grant_count": d.Count, "purchases": json.RawMessage(d.Purchases),
	}
	if subID != "" || refundPay != "" {
		rec := recommend.CancelAndRefundRec(subID, refundPay)
		ev[recommend.EvidenceKey] = rec.Map()
	}
	return ConvergeFinding{
		Type:              "consistency.duplicate.ownership",
		Shape:             ShapeExcess,
		Class:             ClassAdmin,
		Severity:          "critical",
		SubjectKey:        "ownership:" + d.CustomerID.String() + ":" + productID,
		Provider:          "self",
		Evidence:          ev,
		RecommendedAction: prose,
		// surface-only: cancelling/refunding a purchase is an operator decision.
	}, nil
}
