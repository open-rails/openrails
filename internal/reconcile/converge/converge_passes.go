package converge

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// deriveBackfillWindow bounds derive-1: only subscriptions/payments whose
// access window ends within this lookback are derived. A past-ended window is
// harmless (not live); the bound keeps a merchant-wide pass cheap.
const deriveBackfillWindow = 3 * 365 * 24 * time.Hour

// convergeScanCap bounds one pass's per-merchant detector scans, so its work
// does not scale with the whole book. Each capped scan is ordered by urgency
// and a pass removes what it repairs, so a merchant over the cap drains from
// the front across passes. Far above any healthy merchant's drift.
const convergeScanCap = 5000

// DERIVE runs before CON: sources must be truthful before grant effects are
// derived, and the final consistency checks read what DERIVE left.

// derivePass is the DERIVE plane (source → grant → grant effect): it drives
// derive-1/derive-2 through the grants package, the sole writer of grants and
// grant effects, and verifies their output against the source ledger.
type derivePass struct{ e *ConvergeEngine }

func (*derivePass) Plane() string { return "DERIVE" }

// Run checks one customer, or every grant of the merchant in one set query
// per check when the scope names none.
func (p *derivePass) Run(ctx context.Context, scope Scope) ([]ConvergeFinding, error) {
	customer := scope.Customer
	// derive.grant_effect.missing — every live grant has its derived effect
	// (entitlement windows / credit deposit). Repair = MaterializeGrant
	// (idempotent).
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
			Evidence:   map[string]any{"grant_id": g.ID.String(), "kind": g.Kind, "customer_id": g.CustomerID.String()},
			Repair:     func(ctx context.Context) error { return p.lockedMaterialize(ctx, scope, g) },
		})
	}

	// derive.grant_effect.excess — a terminated grant whose effect was never
	// retracted (a recorded revoke/expire that didn't propagate). Repair =
	// MaterializeGrant (entitlement revoke / credit clawback). Grant and
	// termination are both present, so this is AUTO propagation of a recorded
	// decision, not the confirmed-absence case.
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
			Evidence:   map[string]any{"grant_id": g.ID.String(), "kind": g.Kind, "customer_id": g.CustomerID.String(), "cause": "terminated_grant"},
			Repair:     func(ctx context.Context) error { return p.lockedMaterialize(ctx, scope, g) },
		})
	}

	// derive.grant.excess (grant tier) — a live grant whose backing payment
	// was refunded. Surface-only ADMIN, not gated: a refund that keeps access
	// (goodwill) is legitimate, so an operator decides; never auto-retracted.
	// A subscription cancel terminates the grant at write time, so it never
	// surfaces here.
	// derive.grant.missing (grant tier) — a completed, positive one-off
	// payment for a product whose spec promises grants, yet no grant ("paid,
	// got nothing"). The product's own spec is the signal, so empty-spec
	// products and fees never flag. ADMIN surface-only: re-granting re-runs
	// derive-1, so an operator investigates. Critical: taking money without
	// delivering access outranks giving content away.
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
			Evidence:   map[string]any{"payment_id": billing.PaymentID(p.ID).String(), "customer_id": p.CustomerID.String(), "amount": strconv.FormatInt(p.Amount, 10), "currency": p.Currency, "cause": "grantable_payment_without_grant"},
			// surface-only: re-granting re-runs derive-1 (product-spec-dependent).
		})
	}

	// derive.subscription.missing — a stored subscription in an
	// access-granting state for a grantable product with no grant. Unlike the
	// payment finding above this is AUTO-repaired: the subscription period is
	// the unambiguous window. A no-op for live subs (they carry their grant);
	// it fires on imported ones.
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
			Evidence:   map[string]any{"subscription_id": billing.SubscriptionID(s.ID).String(), "customer_id": s.CustomerID.String(), "status": string(s.Status), "cause": "subscription_without_grant"},
			Repair:     func(ctx context.Context) error { return p.lockedDeriveSubscription(ctx, scope, s) },
		})
	}

	// derive.wallet.missing — a completed solana wallet payment with a stored
	// access window and no grant. AUTO-repaired (the stored expiration is the
	// window). A no-op for live payments; it fires on imported ones.
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
			Evidence:   map[string]any{"payment_id": billing.PaymentID(w.ID).String(), "customer_id": w.CustomerID.String(), "cause": "wallet_payment_without_grant"},
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
			pid = billing.PaymentID(*g.PaymentID).String()
		}
		out = append(out, ConvergeFinding{
			Type:       "derive.grant.excess",
			Shape:      ShapeExcess,
			Class:      ClassAdmin,
			Severity:   "high",
			SubjectKey: "grant:" + g.ID.String(),
			Provider:   "self",
			Evidence:   map[string]any{"grant_id": g.ID.String(), "kind": g.Kind, "customer_id": g.CustomerID.String(), "payment_id": pid, "cause": "refunded_payment"},
			// surface-only: revoking access on a refund is an operator decision.
		})
	}

	// derive.grant_effect.mismatch — subscription ↔ product-access drift, both
	// directions. Grant direction: an `active` sub in a running period whose
	// product was never projected for this period (revoked windows are recorded
	// decisions and count as projected; no-grant subs belong to
	// derive.subscription.missing). Repair = derive-1; grants stay the sole
	// effect writer.
	q := p.e.DB.Gen(ctx)
	now := p.e.Now()
	unprojected, err := q.ListActiveSubsMissingAccessProjection(ctx, gen.ListActiveSubsMissingAccessProjectionParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: customer, Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("derive: scan unprojected subscriptions: %w", err)
	}
	unprojectedSubscriptions := make(map[uuid.UUID]struct{}, len(unprojected))
	for i := range unprojected {
		s := unprojected[i]
		unprojectedSubscriptions[s.ID] = struct{}{}
		out = append(out, ConvergeFinding{
			Type:       "derive.grant_effect.mismatch",
			Shape:      ShapeMismatch,
			Class:      ClassAuto,
			Severity:   "high",
			SubjectKey: "subscription:" + s.ID.String(),
			Provider:   "self",
			Evidence: map[string]any{
				"subscription_id": billing.SubscriptionID(s.ID).String(), "customer_id": s.CustomerID.String(),
				"direction": "grant", "product_id": billing.ProductID(s.ProductID).String(),
			},
			Repair: func(ctx context.Context) error {
				return p.lockedDeriveSubscription(ctx, scope, gen.ListUngrantedSubscriptionsRow(s))
			},
		})
	}

	// A chargeback closes access; ordinary cancellation keeps purchased grants.
	dead, err := q.ListDeadSubsWithLiveAccess(ctx, gen.ListDeadSubsWithLiveAccessParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: customer, Now: now, RowLimit: convergeScanCap,
	})
	if err != nil {
		return nil, fmt.Errorf("derive: scan dead subscriptions with live access: %w", err)
	}
	markTruncated(ctx, len(dead), "derive.grant_effect.mismatch")
	for i := range dead {
		d := dead[i]
		closeAt := now // no recorded bound: close at detection time
		evidence := map[string]any{
			"subscription_id": billing.SubscriptionID(d.ID).String(), "customer_id": d.CustomerID.String(),
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
			Retracts:   true,
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

	// derive.access.unjustified — the FREELOADER detector: a live product-access
	// window whose source is proven absent or reversed (sub row missing;
	// refunded one-off payment) with no live grant justifying it. Stale is not
	// freeloading: standing windows of live/unknown/past_due subs never surface
	// here. ADMIN surface-only (access removal is an operator decision), with
	// the revoke/grant recommendation pair.
	unjustified, err := q.ListUnjustifiedAccessWindows(ctx, gen.ListUnjustifiedAccessWindowsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: customer, Now: now, RowLimit: convergeScanCap,
	})
	if err != nil {
		return nil, fmt.Errorf("derive: scan unjustified access windows: %w", err)
	}
	markTruncated(ctx, len(unjustified), reconcile.FindingAccessUnjustified)
	for i := range unjustified {
		out = append(out, unjustifiedAccessFinding(&unjustified[i]))
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

// unjustifiedAccessFinding renders one policy-ambiguous freeloader window as
// an ADMIN finding recommending revoke (default) or a free product grant.
func unjustifiedAccessFinding(o *gen.ListUnjustifiedAccessWindowsRow) ConvergeFinding {
	product := billing.ProductID(o.ProductID)
	alt := recommend.GrantProductRec(billing.CustomerID(o.CustomerID), product, "known-legitimate access")
	rec := recommend.RevokeProductAccessRec(o.AccessID.String(), "", &alt)

	ev := map[string]any{
		"access_id": o.AccessID.String(), "customer_id": o.CustomerID.String(),
		"product_id": product.String(), "source_type": o.SourceType, "source_id": billing.SourceRef(o.SourceType, o.SourceID),
		"cause":               o.Cause,
		recommend.EvidenceKey: rec.Map(),
	}
	if o.PaymentID != nil {
		ev["payment_id"] = o.PaymentID.String()
	}

	var prose string
	switch o.Cause {
	case "missing_subscription":
		prose = fmt.Sprintf("Live access to product %s for customer %s references subscription %s, which does not exist — access has no justification. Revoke the window, or grant the product if it is known-legitimate.",
			product, o.CustomerID, o.SourceID)
	default: // refunded_payment
		prose = fmt.Sprintf("Live access to product %s is sourced by refunded payment %s with no live grant justifying it. Revoke the window, or grant the product if it is known-legitimate.",
			product, o.SourceID)
	}

	return ConvergeFinding{
		Type:              reconcile.FindingAccessUnjustified,
		Shape:             ShapeExcess,
		Class:             ClassAdmin,
		Severity:          "high",
		SubjectKey:        "product_access:" + o.AccessID.String(),
		Provider:          "self",
		Evidence:          ev,
		RecommendedAction: prose,
		// surface-only (policy): never auto-revoke on a derived conclusion.
	}
}

// lockedMaterialize runs a MaterializeGrant repair inside a merchant tx under
// the per-customer spend lock: the credit legs (deposit / clawback) are
// check-then-write, so overlapping converge runs must serialize with each
// other and with spends. Entitlement/ownership legs are cheap no-ops when
// already projected.
func (p *derivePass) lockedMaterialize(ctx context.Context, scope Scope, g gen.BillingGrant) error {
	ctx = merchant.WithID(ctx, scope.Merchant)
	return p.e.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		gl := grants.New(q, scope.Merchant.UUID())
		gl.SetClock(p.e.Now)
		if err := gl.LockCustomer(ctx, g.CustomerID); err != nil {
			return err
		}
		if g.Kind == string(grants.Access) && g.SourceType == string(grants.Subscription) {
			closed, err := q.AccessGrantPastChargebackBoundary(ctx, gen.AccessGrantPastChargebackBoundaryParams{
				MerchantID: scope.Merchant.UUID(), GrantID: g.ID,
			})
			if err != nil {
				return err
			}
			if closed {
				return nil
			}
		}
		return gl.MaterializeGrant(ctx, g)
	})
}

// Subscription repair shares the customer's lock with refund revocation.
func (p *derivePass) lockedDeriveSubscription(ctx context.Context, scope Scope, sub gen.ListUngrantedSubscriptionsRow) error {
	ctx = merchant.WithID(ctx, scope.Merchant)
	return p.e.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		gl := grants.New(gen.New(tx), scope.Merchant.UUID())
		gl.SetClock(p.e.Now)
		if err := gl.LockCustomer(ctx, sub.CustomerID); err != nil {
			return err
		}
		return gl.DeriveSubscriptionGrant(ctx, sub)
	})
}

// healthPass is the merchant's health: the recovery funnel, held renewals,
// payment health, the unverified backlog and provider operations that are
// stuck or abandoned. Merchant scope only.
type healthPass struct{ e *ConvergeEngine }

// abandonedLookback is how far back an operation that ended failed or
// expired is reported: past the hourly pass, with room for an outage.
const abandonedLookback = 25 * time.Hour

func (p *healthPass) Run(ctx context.Context, scope Scope) ([]ConvergeFinding, error) {
	q := p.e.DB.Gen(ctx)
	now := p.e.Now()
	mid := scope.Merchant.UUID()
	out, err := p.unverifiedFindings(ctx, scope, now)
	if err != nil {
		return nil, err
	}
	funnel, err := p.funnelFinding(ctx, scope, now)
	if err != nil {
		return nil, err
	}
	if funnel != nil {
		out = append(out, *funnel)
	}
	held, err := p.heldRenewalsFinding(ctx, scope, now)
	if err != nil {
		return nil, err
	}
	if held != nil {
		out = append(out, *held)
	}
	health, err := p.paymentHealthFindings(ctx, now)
	if err != nil {
		return nil, err
	}
	out = append(out, health...)

	// life.provider_intent.abandoned — a desired provider action that will not
	// auto-retry (ended failed or expired, or past its deadline) needs a human.
	// Surface-only; it closes once its operation is no longer abandoned.
	abandoned, err := q.ListAbandonedProviderIntents(ctx, gen.ListAbandonedProviderIntentsParams{
		MerchantID: mid, Since: now.Add(-abandonedLookback), Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("health: scan abandoned provider intents: %w", err)
	}
	for i := range abandoned {
		pi := abandoned[i]
		out = append(out, ConvergeFinding{
			Type:       findingIntentAbandoned,
			Shape:      ShapeMismatch,
			Class:      ClassAdmin,
			Severity:   "high",
			SubjectKey: "provider_intent:" + pi.ID.String(),
			Provider:   "self",
			Evidence:   map[string]any{"provider_intent_id": pi.ID.String(), "intent_type": pi.IntentType, "intent_status": pi.Status, "provider": pi.Rail},
		})
	}
	if _, err := q.AutoResolveRecoveredAbandonedIntentFindings(ctx, gen.AutoResolveRecoveredAbandonedIntentFindingsParams{MerchantID: mid, Now: now}); err != nil {
		return nil, fmt.Errorf("health: resolve recovered abandoned-intent findings: %w", err)
	}

	// life.provider_intent.stuck — a rail intent sitting non-terminal beyond
	// the stuck thresholds. Mode/kill-switch parks are informational (the
	// intent's job runs it once the blocker lifts); anything else means
	// provider failures, bad credentials or a lost job. Never repaired here:
	// the intent's job owns it.
	actionCutoff, verifyCutoff := now.Add(-stuckActionableAge), now.Add(-stuckVerifyAge)
	stuck, err := q.ListStuckProviderIntents(ctx, gen.ListStuckProviderIntentsParams{
		MerchantID: mid, ActionCutoff: actionCutoff, VerifyCutoff: verifyCutoff, Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("health: scan stuck rail intents: %w", err)
	}
	for i := range stuck {
		out = append(out, stuckIntentFinding(&stuck[i], now))
	}
	// Stuck findings close subject-first once their intent no longer meets
	// the stuck criteria.
	if _, err := q.AutoResolveRecoveredStuckIntentFindings(ctx, gen.AutoResolveRecoveredStuckIntentFindingsParams{
		MerchantID: mid, ActionCutoff: actionCutoff, VerifyCutoff: verifyCutoff, Now: now,
	}); err != nil {
		return nil, fmt.Errorf("health: resolve recovered stuck-intent findings: %w", err)
	}
	return out, nil
}

// Stuck-intent thresholds — HARDCODED (no-knobs policy): each intent's job
// wakes at its next attempt, so a day-old actionable intent means provider
// failures, bad credentials, a lost job — or a deliberate park; a healthy job
// resolves an unknown in minutes, and an in_flight lease outliving hours
// means its job died.
const (
	stuckActionableAge = 24 * time.Hour // pending / failed_retryable
	stuckVerifyAge     = 2 * time.Hour  // in_flight / unknown_needs_verify
)

// isModeParkedReason reports whether last_failure_reason records a deliberate
// park by the operating mode / kill switch (intents.GateExecution "mode=..."
// strings). Parked is not broken — each intent's job runs it when the
// blocker lifts — so the finding is informational, not the admin queue.
func isModeParkedReason(reason string) bool { return strings.Contains(reason, "mode=") }

// stuckIntentFinding diagnoses one stuck rail intent. SubjectKey is the bare
// intent id; Provider is the intent's own rail.
func stuckIntentFinding(si *gen.BillingProviderIntent, now time.Time) ConvergeFinding {
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

// accessLapseLookback bounds the hourly pass's access-ended scan: timed
// windows that lapsed since the pass before last, with room for an outage.
const accessLapseLookback = 25 * time.Hour

// notifyPass is the NOTIFY plane: whatever plane closed a customer's last
// entitlement window (dunning, reconcile-driven cancel, grant lapse), the
// customer is told exactly once. It only records premium_ended notification
// events; the host's applications deliver them. Dedupe: any premium_ended
// event at or after the close means a transition site already told them.
type notifyPass struct {
	e        *ConvergeEngine
	lookback time.Duration
}

func (*notifyPass) Plane() string { return "NOTIFY" }
func (p *notifyPass) Run(ctx context.Context, scope Scope) ([]ConvergeFinding, error) {
	q := p.e.DB.Gen(ctx)
	now := p.e.Now()
	closed, err := q.ListRecentlyClosedLastAccessWindows(ctx, gen.ListRecentlyClosedLastAccessWindowsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: scope.Customer,
		ClosedAfter: now.Add(-p.lookback), Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("notify: scan recently closed access windows: %w", err)
	}
	var out []ConvergeFinding
	repo := subscriptions.NewNotificationService(p.e.DB)
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
		cust, product := c.CustomerID, billing.ProductID(c.ProductID)
		var subscriptionID billing.SubscriptionID
		if c.SourceType == "subscription" || c.SourceType == "grace" {
			if id, err := uuid.Parse(c.SourceID); err == nil {
				subscriptionID = billing.SubscriptionID(id)
			}
		}
		out = append(out, ConvergeFinding{
			Type:       "notify.access_ended.missing",
			Shape:      ShapeMissing,
			Class:      ClassAuto,
			Severity:   "low",
			SubjectKey: "customer:" + cust.String(),
			Provider:   "self",
			Evidence: map[string]any{
				"customer_id": cust.String(), "product_id": product.String(),
				"ended_at": closedAt.Format(time.RFC3339), "source_type": c.SourceType, "source_id": billing.SourceRef(c.SourceType, c.SourceID),
			},
			Repair: func(ctx context.Context) error {
				ctx = merchant.WithID(ctx, scope.Merchant)
				return repo.CreateIfAbsent(ctx, &models.NotificationEvent{
					ID:         uuid.NewSHA1(cust, []byte("access-ended:"+closedAt.Format(time.RFC3339Nano))),
					CustomerID: cust,
					EventType:  models.NotificationPremiumEnded,
					CreatedAt:  now,
					Data: billing.NotificationData{
						Reason:         string(subscriptions.PremiumEndReasonAccessEnded),
						EndedAt:        &closedAt,
						AccessEnded:    true,
						ProductID:      product,
						Source:         "converge_notify",
						SubscriptionID: subscriptionID,
					},
				})
			},
		})
	}
	return out, nil
}

// conPass — CON plane: residual internal consistency (duplicate / reference).
type conPass struct{ e *ConvergeEngine }

func (*conPass) Plane() string { return "CON" }
func (p *conPass) Run(ctx context.Context, scope Scope) ([]ConvergeFinding, error) {
	// CON holds the residual accounting/referential checks that are not a DB
	// constraint, a lifecycle transition or a DERIVE grant effect. Its findings are
	// ADMIN and surface-only: a dangling reference or a duplicate has no safe
	// automatic repair.
	//
	// consistency.reference.source_reference — an entitlement's polymorphic
	// source_type/source_id pair resolves to no row (or to one in the wrong
	// merchant). EXCESS/MISMATCH → ADMIN.
	q := p.e.DB.Gen(ctx)
	now := p.e.Now()
	var out []ConvergeFinding

	// Customer-scoped when scope.Customer is set, merchant-wide when nil; the
	// SQL filters, so an after-every-mutation run stays cheap. Live windows
	// with dangling sub sources are freeloaders (derive.access.unjustified);
	// this check keeps the non-live rest.
	cust := scope.Customer
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	orphanSubs, err := q.ConOrphanAccessSubscriptionSource(ctx, gen.ConOrphanAccessSubscriptionSourceParams{
		MerchantID: scopeMerchantID.UUID(),
		Now:        now, CustomerID: cust,
	})
	if err != nil {
		return nil, fmt.Errorf("con: scan orphan subscription sources: %w", err)
	}
	orphanPays, err := q.ConOrphanAccessPaymentSource(ctx, gen.ConOrphanAccessPaymentSourceParams{MerchantID: scopeMerchantID.UUID(), CustomerID: cust})
	if err != nil {
		return nil, fmt.Errorf("con: scan orphan payment sources: %w", err)
	}
	// Free grants have no source row to dangle: the grant ledger is their
	// provenance.

	emit := func(accessID uuid.UUID, userID string, product uuid.UUID, sourceType, sourceID string) {
		out = append(out, ConvergeFinding{
			Type:       "consistency.reference.source_reference",
			Shape:      ShapeMismatch,
			Class:      ClassAdmin,
			Severity:   "medium",
			SubjectKey: "product_access:" + accessID.String(),
			Provider:   "self",
			Evidence: map[string]any{
				"access_id": accessID.String(), "customer_id": userID,
				"product_id": billing.ProductID(product).String(), "source_type": sourceType, "source_id": billing.SourceRef(sourceType, sourceID),
			},
			// surface-only: a dangling source has no safe auto-repair (it may be a
			// valid historical record or a real corruption) — an admin decides.
		})
	}
	for i := range orphanSubs {
		r := orphanSubs[i]
		emit(r.AccessID, r.UserID, r.ProductID, r.SourceType, r.SourceID)
	}
	for i := range orphanPays {
		r := orphanPays[i]
		emit(r.AccessID, r.UserID, r.ProductID, r.SourceType, r.SourceID)
	}

	// consistency.duplicate.provider_charge — more than one captured charge
	// for one period of one subscription (the period each charge paid for, or
	// for older rows the subscription's cadence). Distinct consecutive periods
	// are never duplicates. EXCESS → ADMIN, surface-only: collecting money
	// twice is never auto-undone (a refund is an operator decision); the
	// finding carries the payment ids and a refund recommendation. Critical:
	// money harm. Findings the scan no longer reports close themselves.
	dupCharges, err := q.ConDuplicateChargesSamePeriod(ctx, gen.ConDuplicateChargesSamePeriodParams{MerchantID: scopeMerchantID.UUID(), CustomerID: cust})
	if err != nil {
		return nil, fmt.Errorf("con: scan duplicate charges: %w", err)
	}
	for i := range dupCharges {
		d := dupCharges[i]
		ids := make([]string, len(d.PaymentIds))
		for j, id := range d.PaymentIds {
			ids[j] = billing.PaymentID(id).String()
		}
		subID := billing.SubscriptionID{}
		if d.SubscriptionID != nil {
			subID = billing.SubscriptionID(*d.SubscriptionID)
		}
		productID := ""
		if d.ProductID != nil {
			productID = billing.ProductID(*d.ProductID).String()
		}
		subject := "provider_charge:" + d.UserID + ":" + subID.String() + ":" + d.PeriodKey
		out = append(out, ConvergeFinding{
			Type:       findingDuplicateCharge,
			Shape:      ShapeExcess,
			Class:      ClassAdmin,
			Severity:   "critical",
			SubjectKey: subject,
			Provider:   "self",
			Evidence: map[string]any{
				"customer_id": d.UserID, "subscription_id": subID.String(), "order_id": d.PeriodKey,
				"product_id": productID, "product_key": d.ProductKey,
				"charge_count": d.Count, "payment_ids": ids, "total_amount": strconv.FormatInt(d.TotalAmount, 10), "currency": d.Currency,
				"first_date": d.FirstDate, "last_date": d.LastDate,
			},
			RecommendedAction: fmt.Sprintf("Customer %s has net captured money exceeding the frozen total of order %s for subscription %s (%q; net %s; payments %s). Review the order and refund only the verified excess; multiple partial payments are not inherently duplicates.",
				d.UserID, d.PeriodKey, subID.String(), d.ProductKey, moneyutil.FormatAmount(d.TotalAmount, d.Currency), strings.Join(ids, ", ")),
			// surface-only: a refund/credit is an operator decision, never automatic.
		})
	}

	// consistency.duplicate.ownership — more than one live paid ownership
	// grant per (customer, product): the cross-month one-off/lifetime double
	// purchase duplicate.provider_charge misses. Critical, ADMIN surface-only,
	// with a cancel_and_refund recommendation targeting the later purchase
	// (override_params can flip it before approving).
	dupOwn, err := q.ConDuplicateOwnershipGrants(ctx, gen.ConDuplicateOwnershipGrantsParams{MerchantID: scopeMerchantID.UUID(), Now: now, CustomerID: cust})
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

// ownershipPurchaseRow is one leg of a duplicate-ownership group as the
// query's jsonb purchases array spells it (bare ids, numeric amount,
// ordered oldest-first); ownershipPurchase is its finding-evidence shape.
type ownershipPurchaseRow struct {
	GrantID     string  `json:"grant_id"`
	SourceType  string  `json:"source_type"`
	SourceID    string  `json:"source_id"`
	PaymentID   *string `json:"payment_id"`
	Amount      *int64  `json:"amount"`
	Currency    *string `json:"currency"`
	PurchasedAt string  `json:"purchased_at"`
}

type ownershipPurchase struct {
	GrantID     string  `json:"grant_id"`
	SourceType  string  `json:"source_type"`
	SourceID    string  `json:"source_id"`
	PaymentID   *string `json:"payment_id"`
	Amount      *int64  `json:"amount,string"`
	Currency    *string `json:"currency"`
	PurchasedAt string  `json:"purchased_at"`
}

func (r ownershipPurchaseRow) evidence() ownershipPurchase {
	p := ownershipPurchase{GrantID: r.GrantID, SourceType: r.SourceType, SourceID: billing.SourceRef(r.SourceType, r.SourceID), Amount: r.Amount, Currency: r.Currency, PurchasedAt: r.PurchasedAt}
	if r.PaymentID != nil {
		if u, err := uuid.Parse(*r.PaymentID); err == nil {
			typed := billing.PaymentID(u).String()
			p.PaymentID = &typed
		} else {
			p.PaymentID = r.PaymentID
		}
	}
	return p
}

func (p ownershipPurchase) describe() string {
	s := "grant " + p.GrantID + " (purchased " + p.PurchasedAt
	if p.PaymentID != nil {
		s += ", payment " + *p.PaymentID
	}
	if p.Amount != nil && p.Currency != nil {
		s += ", " + moneyutil.FormatAmount(*p.Amount, *p.Currency)
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
	var rows []ownershipPurchaseRow
	if err := json.Unmarshal(d.Purchases, &rows); err != nil {
		return ConvergeFinding{}, fmt.Errorf("con: decode duplicate ownership purchases: %w", err)
	}
	purchases := make([]ownershipPurchase, len(rows))
	for i, row := range rows {
		purchases[i] = row.evidence()
	}
	var subID billing.SubscriptionID
	var refundPay billing.PaymentID
	later := purchases[len(purchases)-1]
	if later.SourceType == "subscription" {
		if id, err := billing.ParseSubscriptionID(later.SourceID); err == nil {
			subID = id
		}
	}
	if later.PaymentID != nil {
		if id, err := billing.ParsePaymentID(*later.PaymentID); err == nil {
			refundPay = id
		}
	}
	productID, subjectProduct := "", ""
	if d.ProductID != nil {
		productID, subjectProduct = billing.ProductID(*d.ProductID).String(), d.ProductID.String()
	}
	purchasesJSON, err := json.Marshal(purchases)
	if err != nil {
		return ConvergeFinding{}, fmt.Errorf("con: encode duplicate ownership purchases: %w", err)
	}

	descs := make([]string, len(purchases))
	for i := range purchases {
		descs[i] = purchases[i].describe()
	}
	prose := fmt.Sprintf("Customer %s holds %d live ownership grants for product %q — charged more than once for the same product: %s. Cancel/refund the later purchase (default) unless the earlier one is the mistake.",
		d.CustomerID, d.Count, d.ProductKey, strings.Join(descs, "; "))

	ev := map[string]any{
		"customer_id": d.CustomerID.String(), "product_id": productID, "product_key": d.ProductKey,
		"grant_count": d.Count, "purchases": json.RawMessage(purchasesJSON),
	}
	if !subID.IsZero() || !refundPay.IsZero() {
		rec := recommend.CancelAndRefundRec(subID, refundPay)
		ev[recommend.EvidenceKey] = rec.Map()
	}
	return ConvergeFinding{
		Type:              "consistency.duplicate.ownership",
		Shape:             ShapeExcess,
		Class:             ClassAdmin,
		Severity:          "critical",
		SubjectKey:        "ownership:" + d.CustomerID.String() + ":" + subjectProduct,
		Provider:          "self",
		Evidence:          ev,
		RecommendedAction: prose,
		// surface-only: cancelling/refunding a purchase is an operator decision.
	}, nil
}

// unverifiedUnresolvedAfter is how long a row may stay unverified before it is
// escalated to the operator (unverified is never a dead end).
const unverifiedUnresolvedAfter = 72 * time.Hour

// unverifiedFindings reports the unverified backlog per account
// (life.unverified.backlog: count and oldest age, every pass, so the operator
// can watch it converge) and escalates each row unresolved past
// unverifiedUnresolvedAfter (life.unverified.unresolved).
func (p *healthPass) unverifiedFindings(ctx context.Context, scope Scope, now time.Time) ([]ConvergeFinding, error) {
	rows, err := p.e.DB.Gen(ctx).ListUnverifiedSubscriptions(ctx, gen.ListUnverifiedSubscriptionsParams{
		MerchantID: scope.Merchant.UUID(), RowLimit: convergeScanCap,
	})
	if err != nil {
		return nil, fmt.Errorf("health: scan unverified subscriptions: %w", err)
	}
	markTruncated(ctx, len(rows), findingUnverifiedUnresolved, findingUnverifiedBacklog)
	type backlog struct {
		count  int
		oldest time.Time
	}
	accounts := map[uuid.UUID]*backlog{}
	var out []ConvergeFinding
	for _, r := range rows {
		b := accounts[r.PspID]
		if b == nil {
			b = &backlog{oldest: r.UnverifiedAt}
			accounts[r.PspID] = b
		}
		b.count++
		if r.UnverifiedAt.Before(b.oldest) {
			b.oldest = r.UnverifiedAt
		}
		if now.Sub(r.UnverifiedAt) < unverifiedUnresolvedAfter {
			continue
		}
		evidence := map[string]any{"subscription_id": billing.SubscriptionID(r.ID).String(), "rail": r.Rail, "unverified_since": r.UnverifiedAt.UTC(), "reads": r.Reads}
		if r.LastReadAt != nil {
			evidence["last_read_at"] = r.LastReadAt.UTC()
		}
		out = append(out, ConvergeFinding{
			Type: findingUnverifiedUnresolved, Shape: ShapeMismatch, Class: ClassAdmin, Severity: SeverityHigh,
			SubjectKey: "subscription:" + r.ID.String(), Provider: "self", Evidence: evidence,
			RecommendedAction: fmt.Sprintf("Subscription %s has been unverified since %s: provider reads found no payment, decline or cancellation to settle it. Access is held. Check the schedule at %s and record the outcome.",
				billing.SubscriptionID(r.ID), r.UnverifiedAt.UTC().Format(time.RFC3339), r.Rail),
		})
	}
	for psp, b := range accounts {
		age := max(now.Sub(b.oldest), 0)
		severity := SeverityLow
		if age >= unverifiedUnresolvedAfter {
			severity = SeverityMedium
		}
		out = append(out, ConvergeFinding{
			Type: findingUnverifiedBacklog, Shape: ShapeMismatch, Class: ClassAuto, Severity: severity,
			SubjectKey: "psp:" + psp.String(), Provider: "self",
			Evidence: map[string]any{"psp_id": psp.String(), "count": b.count, "oldest_since": b.oldest.UTC(), "oldest_age_seconds": int64(age.Seconds())},
		})
	}
	return out, nil
}

// Finding types this package names in more than one place.
const (
	findingIntentAbandoned      = "life.provider_intent.abandoned"
	findingUnverifiedBacklog    = "life.unverified.backlog"
	findingUnverifiedUnresolved = "life.unverified.unresolved"
	findingDunningFunnel        = "life.dunning.funnel"
	findingRenewalHeld          = "life.renewal.held"
	findingDuplicateCharge      = "consistency.duplicate.provider_charge"
)

func (*derivePass) Standing() []string {
	return []string{"derive.grant_effect.missing", "derive.grant_effect.excess", "derive.grant.missing", "derive.grant.excess",
		"derive.subscription.missing", "derive.wallet.missing", "derive.grant_effect.mismatch", reconcile.FindingAccessUnjustified}
}

// Standing leaves out the stuck and abandoned provider operations, which
// resolve by their own criteria.
func (*healthPass) Standing() []string {
	return []string{findingUnverifiedBacklog, findingUnverifiedUnresolved, findingDunningFunnel, findingRenewalHeld,
		findingNewCardDeclineSpike, findingRebillFailureSpike, findingSystemErrors, findingDeclineUnmapped, findingWebhookSilence}
}

// HealthFindingTypes are the finding types Health reports: a merchant with one
// open is looked at again until it closes.
func HealthFindingTypes() []string {
	return append((*healthPass)(nil).Standing(), findingIntentAbandoned, "life.provider_intent.stuck")
}

func (*notifyPass) Standing() []string { return []string{"notify.access_ended.missing"} }

func (*conPass) Standing() []string {
	return []string{"consistency.reference.source_reference", findingDuplicateCharge, "consistency.duplicate.ownership"}
}

// heldRenewalsFinding reports engine renewals with no outcome past their
// allowance: collection is stopped (fleet halted, admission hold, breaker,
// readonly). Members keep access by default, so the backlog is the operator's
// signal. It resolves once collection resumes and the renewals are attempted.
func (p *healthPass) heldRenewalsFinding(ctx context.Context, scope Scope, now time.Time) (*ConvergeFinding, error) {
	h, err := p.e.DB.Gen(ctx).SummarizeHeldEngineRenewals(ctx, gen.SummarizeHeldEngineRenewalsParams{MerchantID: scope.Merchant.UUID(), Now: now})
	if err != nil {
		return nil, fmt.Errorf("life: summarize held renewals: %w", err)
	}
	if h.Held == 0 {
		return nil, nil
	}
	age := max(now.Sub(h.OldestDueAt), 0)
	return &ConvergeFinding{
		Type: findingRenewalHeld, Shape: ShapeMismatch, Class: ClassOperator, Severity: SeverityHigh,
		SubjectKey: "engine", Provider: "self",
		Evidence: map[string]any{"count": h.Held, "oldest_paid_through": h.OldestDueAt.UTC(), "oldest_held_seconds": int64(age.Seconds())},
		RecommendedAction: fmt.Sprintf("%d engine renewals have had no outcome past their allowance (oldest due %s): collection is stopped. Members keep access until they are attempted. Resume collection (fleet, admission hold, breaker, provider write mode).",
			h.Held, h.OldestDueAt.UTC().Format(time.RFC3339)),
	}, nil
}

// funnelFinding is the merchant's recovery funnel: live subscriptions by
// state, the oldest unverified entry and open unknown provider operations
// (unmapped decline codes are life.decline.unmapped). Raised only while
// something is in recovery or needs attention; resolves when the funnel is
// empty.
func (p *healthPass) funnelFinding(ctx context.Context, scope Scope, now time.Time) (*ConvergeFinding, error) {
	q := p.e.DB.Gen(ctx)
	mid := scope.Merchant.UUID()
	f, err := q.CountSubscriptionFunnel(ctx, gen.CountSubscriptionFunnelParams{MerchantID: mid, Now: now})
	if err != nil {
		return nil, fmt.Errorf("life: count subscription funnel: %w", err)
	}
	unknown, err := q.CountUnknownOperations(ctx, gen.CountUnknownOperationsParams{MerchantID: mid, Now: now})
	if err != nil {
		return nil, fmt.Errorf("life: count unknown operations: %w", err)
	}
	if f.PastDue+f.AwaitingMethod+f.Unverified+unknown.OpenCount == 0 {
		return nil, nil
	}
	evidence := map[string]any{
		"subscriptions":      funnelStates{Active: f.Active, PastDue: f.PastDue, AwaitingMethod: f.AwaitingMethod, Unverified: f.Unverified},
		"unknown_operations": unknown.OpenCount,
	}
	severity := SeverityLow
	if f.Unverified > 0 {
		evidence["oldest_unverified_age_seconds"] = f.OldestUnverifiedAgeSeconds
		if time.Duration(f.OldestUnverifiedAgeSeconds)*time.Second >= unverifiedUnresolvedAfter {
			severity = SeverityMedium
		}
	}
	if unknown.OpenCount > 0 {
		evidence["oldest_unknown_operation_age_seconds"] = unknown.OldestAgeSeconds
		if time.Duration(unknown.OldestAgeSeconds)*time.Second >= stuckVerifyAge {
			severity = SeverityMedium
		}
	}
	return &ConvergeFinding{
		Type: findingDunningFunnel, Shape: ShapeMismatch, Class: ClassAuto, Severity: severity,
		SubjectKey: "merchant:" + mid.String(), Provider: "self", Evidence: evidence,
	}, nil
}

// funnelStates is the funnel's live subscriptions by lifecycle state.
type funnelStates struct {
	Active         int64 `json:"active"`
	PastDue        int64 `json:"in_dunning"` // past_due; the key avoids the money-name guard
	AwaitingMethod int64 `json:"awaiting_method"`
	Unverified     int64 `json:"unverified"`
}

// samePeriod reports whether the locked row is still on the scanned period.
func samePeriod(current, scanned *time.Time) bool {
	if current == nil || scanned == nil {
		return current == scanned
	}
	return current.Equal(*scanned)
}
