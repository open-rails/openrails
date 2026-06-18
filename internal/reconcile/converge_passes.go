package reconcile

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/grants"
)

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
	// derive.grant_effect.missing — every live grant has its derived effect
	// (entitlement windows / credit deposit). Repair = MaterializeGrant (#514,
	// idempotent). Customer-scoped for now; merchant-wide enumeration + the
	// remaining DERIVE checks (grant.*, grant_effect.excess/mismatch) follow.
	if scope.Customer == nil {
		return nil, nil
	}
	gl := grants.New(p.e.DB.Gen(ctx), scope.Merchant.UUID())
	gl.SetClock(p.e.Now)
	missing, err := gl.MissingEffects(ctx, *scope.Customer)
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
	unretracted, err := gl.UnretractedTerminations(ctx, *scope.Customer)
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
				if _, e := q.ReconcileCancelSubscriptionLocal(ctx, gen.ReconcileCancelSubscriptionLocalParams{
					ID: subID, Now: now, CancelType: "past_due", Reason: "grace exhausted (converged)",
				}); e != nil {
					return e
				}
				_, e := q.ReconcileRevokeSubscriptionEntitlements(ctx, gen.ReconcileRevokeSubscriptionEntitlementsParams{
					SubscriptionID: subID, Now: asOf, Reason: "grace exhausted",
				})
				return e
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
	// Phase D: duplicate / amount-mismatch / reference-resolution checks.
	return nil, nil
}
