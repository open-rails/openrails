package converge

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	repo "github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// pendingStaleAfter is how long a `pending` subscription may sit unconfirmed
// before life.subscription.pending_stale terminates it. Conservative: well beyond
// any real activation latency, so auto-cancelling can't race a confirming sub.
const pendingStaleAfter = 72 * time.Hour

// periodGrace is the grace window appended to a missed period end when
// life.subscription.period_overdue converges an active sub into dunning. Matches
// the dunning grace cap (subscriptions.graceSlackCap = 48h).
const periodGrace = 48 * time.Hour

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
			Repair:     func(ctx context.Context) error { return gl.MaterializeGrant(ctx, g) },
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
			Type:       "derive.grant_effect.excess",
			Shape:      ShapeExcess,
			Class:      ClassAuto,
			Severity:   "high",
			SubjectKey: "grant_effect:" + g.ID.String(),
			Provider:   "self",
			Evidence:   map[string]any{"grant_id": g.ID.String(), "kind": g.Kind, "cause": "terminated_grant"},
			Repair:     func(ctx context.Context) error { return gl.MaterializeGrant(ctx, g) },
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
			Severity:   "high",
			SubjectKey: "payment:" + p.ID.String(),
			Provider:   "self",
			Evidence:   map[string]any{"payment_id": p.ID.String(), "amount": p.Amount, "currency": p.Currency, "cause": "grantable_payment_without_grant"},
			// surface-only: re-granting re-runs derive-1 (product-spec-dependent).
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
	return out, nil
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
			Type:       "life.checkout_session.stale",
			Shape:      ShapeExcess,
			Class:      ClassAuto,
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

	// life.subscription.grace_exhausted — a past_due sub whose grace window has
	// elapsed is terminal. Converge-NOT-replay: cancel NOW, but revoke the
	// entitlements as-of when grace ended (the access truly lapsed then), never
	// re-running the missed dunning charges. Time-driven EXCESS → AUTO, not gated.
	// (A provider cancel action is the linked remediation; the provider-intent
	// enqueue lands with the provider-action wiring.)
	exhausted, err := q.ListGraceExhaustedSubscriptions(ctx, gen.ListGraceExhaustedSubscriptionsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: scope.Customer, Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("life: scan grace-exhausted subscriptions: %w", err)
	}
	for i := range exhausted {
		subID := exhausted[i].ID
		asOf := now
		if exhausted[i].GraceEndsAt != nil {
			asOf = *exhausted[i].GraceEndsAt
		}
		out = append(out, ConvergeFinding{
			Type:       "life.subscription.grace_exhausted",
			Shape:      ShapeExcess,
			Class:      ClassAuto,
			Severity:   "high",
			SubjectKey: "subscription:" + subID.String(),
			Provider:   "self",
			Evidence:   map[string]any{"subscription_id": subID.String(), "grace_ends_at": asOf},
			Repair: func(ctx context.Context) error {
				// Terminal cancel through the shared local-state core: status flip +
				// Solana cranker cascade (#264) + revoke the sub's paid AND grace
				// windows AS-OF grace end (converge-not-replay — access lapsed then,
				// no missed dunning charges re-run). ended_at = now (>= cancelled_at,
				// per chk_ended_not_before_cancelled). No side-effects fired.
				sub, err := repo.NewSubscriptionRepo(p.e.DB).GetByID(ctx, subID)
				if err != nil {
					return fmt.Errorf("life: load grace-exhausted subscription %s: %w", subID, err)
				}
				fb := "grace exhausted (converged)"
				return p.e.lifecycle.ApplyLocalCancellation(ctx, p.e.DB, sub, subscriptions.LocalCancellation{
					EndedAt:       now,
					CancelType:    models.CancelTypeExpired,
					Feedback:      &fb,
					RevokeReason:  models.EntitlementRevokeDunning,
					RevokeAsOf:    asOf,
					RevokeSources: []models.EntitlementSourceType{models.EntitlementSourceSubscription, models.EntitlementSourceGrace},
				})
			},
		})
	}

	// life.subscription.period_overdue — an `active` sub past its period end that
	// never advanced. Converge it into dunning (past_due) with a grace window
	// dated to the period end; a long-overdue sub then terminates via
	// grace_exhausted on the next pass. MISMATCH, time-driven → AUTO, not gated.
	overdue, err := q.ListPeriodOverdueSubscriptions(ctx, gen.ListPeriodOverdueSubscriptionsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: scope.Customer, Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("life: scan period-overdue subscriptions: %w", err)
	}
	for i := range overdue {
		subID := overdue[i].ID
		graceEnds := now
		if overdue[i].CurrentPeriodEndsAt != nil {
			graceEnds = overdue[i].CurrentPeriodEndsAt.Add(periodGrace)
		}
		out = append(out, ConvergeFinding{
			Type:       "life.subscription.period_overdue",
			Shape:      ShapeMismatch,
			Class:      ClassAuto,
			Severity:   "medium",
			SubjectKey: "subscription:" + subID.String(),
			Provider:   "self",
			Evidence:   map[string]any{"subscription_id": subID.String(), "grace_ends_at": graceEnds},
			Repair: func(ctx context.Context) error {
				// Enter dunning via the shared local-state core: active → past_due
				// with a grace window dated to the missed period end (a long-overdue
				// sub then terminates via grace_exhausted next pass).
				sub, err := repo.NewSubscriptionRepo(p.e.DB).GetByID(ctx, subID)
				if err != nil {
					return fmt.Errorf("life: load period-overdue subscription %s: %w", subID, err)
				}
				return p.e.lifecycle.ApplyLocalPastDue(ctx, p.e.DB, sub, graceEnds)
			},
		})
	}

	// life.subscription.pending_stale — a `pending` sub that never confirmed
	// within the threshold is abandoned; cancel it (no entitlements/money to
	// unwind). Time-driven EXCESS → AUTO, not gated.
	stalePending, err := q.ListStalePendingSubscriptions(ctx, gen.ListStalePendingSubscriptionsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: scope.Customer, Cutoff: now.Add(-pendingStaleAfter),
	})
	if err != nil {
		return nil, fmt.Errorf("life: scan stale pending subscriptions: %w", err)
	}
	for i := range stalePending {
		subID := stalePending[i]
		out = append(out, ConvergeFinding{
			Type:       "life.subscription.pending_stale",
			Shape:      ShapeExcess,
			Class:      ClassAuto,
			Severity:   "low",
			SubjectKey: "subscription:" + subID.String(),
			Provider:   "self",
			Evidence:   map[string]any{"subscription_id": subID.String()},
			Repair: func(ctx context.Context) error {
				// Terminal cancel through the shared core. A never-confirmed pending
				// sub has no entitlements/money to unwind (RevokeSources empty); the
				// Solana cascade is a tolerant no-op when no row was ever enrolled.
				sub, err := repo.NewSubscriptionRepo(p.e.DB).GetByID(ctx, subID)
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
	// action). provider_intents are merchant-level (no customer_id), so this runs
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
				Evidence:   map[string]any{"provider_intent_id": pi.ID.String(), "intent_type": pi.IntentType, "intent_status": pi.Status, "provider": pi.Provider},
				// surface-only: no Repair (operator/provider action resolves it)
			})
		}
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
	var out []ConvergeFinding

	// Customer-scoped when scope.Customer is set (inline Converge(customer) → the
	// scan is O(that customer)), merchant-wide when nil (the sweep). The SQL does
	// the filtering, so an after-every-mutation invocation stays cheap.
	cust := scope.Customer
	orphanSubs, err := q.ConOrphanEntitlementSubscriptionSource(ctx, cust)
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
	// carries the duplicate payment ids for that decision.
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
		out = append(out, ConvergeFinding{
			Type:       "consistency.duplicate.provider_charge",
			Shape:      ShapeExcess,
			Class:      ClassAdmin,
			Severity:   "high",
			SubjectKey: "provider_charge:" + d.UserID + ":" + d.ProductID.String() + ":" + d.FirstDate.Format("2006-01"),
			Provider:   "self",
			Evidence: map[string]any{
				"customer_id": d.UserID, "product_id": d.ProductID.String(), "product_key": d.ProductKey,
				"charge_count": d.Count, "payment_ids": ids, "total_amount": d.TotalAmount,
				"first_date": d.FirstDate, "last_date": d.LastDate,
			},
			// surface-only: a refund/credit is an operator decision, never automatic.
		})
	}
	return out, nil
}
