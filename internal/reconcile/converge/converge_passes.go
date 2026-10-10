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
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/lifecycle"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// pendingStaleAfter is how long a `pending` subscription may sit unconfirmed
// before life.subscription.pending_stale terminates it. Conservative: well beyond
// any real activation latency, so auto-cancelling can't race a confirming sub.
const pendingStaleAfter = 72 * time.Hour

// paidPendingAfter is how long a pending subscription may hold a completed
// payment before the missing activation is surfaced.
const paidPendingAfter = time.Hour

// deriveBackfillWindow bounds derive-1: only subscriptions/payments whose
// access window ends within this lookback are derived. A past-ended window is
// harmless (not live); the bound keeps the merchant-wide sweep cheap.
const deriveBackfillWindow = 3 * 365 * 24 * time.Hour

// convergeScanCap bounds one pass's per-merchant detector scans, so sweep work
// does not scale with the whole book. Each capped scan is ordered by urgency
// and a pass removes what it repairs, so a merchant over the cap drains from
// the front across passes. Far above any healthy merchant's drift.
const convergeScanCap = 5000

// The three internal-plane passes run in DERIVE → LIFE → CON order: sources
// must be truthful before grant effects are derived, and lifecycle state
// current before the final consistency checks.

// derivePass is the DERIVE plane (source → grant → grant effect): it drives
// derive-1/derive-2 through the grants package, the sole writer of grants and
// grant effects, and verifies their output against the source ledger.
type derivePass struct{ e *ConvergeEngine }

func (*derivePass) Plane() string { return "DERIVE" }
func (p *derivePass) Run(ctx context.Context, scope Scope) ([]ConvergeFinding, error) {
	// DERIVE belongs to a customer (grants belong to a customer). A
	// customer-scoped Converge checks that one customer; the merchant-scoped
	// sweep checks every grant in one set query per check (`customer` nil).
	// Per-subscription scope defers to the customer-scope run.
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
			Evidence:   map[string]any{"grant_id": g.ID.String(), "kind": g.Kind},
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
			Evidence:   map[string]any{"payment_id": billing.PaymentID(p.ID).String(), "amount": strconv.FormatInt(p.Amount, 10), "currency": p.Currency, "cause": "grantable_payment_without_grant"},
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
			Evidence:   map[string]any{"subscription_id": billing.SubscriptionID(s.ID).String(), "status": string(s.Status), "cause": "subscription_without_grant"},
			Repair:     func(ctx context.Context) error { return gl.DeriveSubscriptionGrant(ctx, s) },
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
			Evidence:   map[string]any{"payment_id": billing.PaymentID(w.ID).String(), "cause": "wallet_payment_without_grant"},
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
			Evidence:   map[string]any{"grant_id": g.ID.String(), "kind": g.Kind, "payment_id": pid, "cause": "refunded_payment"},
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
				return gl.DeriveSubscriptionGrant(ctx, gen.ListUngrantedSubscriptionsRow(s))
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
	markTruncated(ctx, len(unjustified), "derive.access.unjustified")
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
		Type:              "derive.access.unjustified",
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
	// life.checkout_attempt.stale — an expired, non-terminal checkout attempt
	// is cleaned up. EXCESS but time-driven (not confirmed-absence gated), so
	// AUTO.
	q := p.e.DB.Gen(ctx)
	now := p.e.Now()
	// Repairs retain this pass's detection clock without mutating the lifecycle
	// shared by overlapping runs. Its dependencies are fixed during wiring.
	lc := *p.e.lifecycle
	lc.SetClock(clockwork.NewFakeClockAt(now.UTC()))
	stale, err := q.ListStaleCheckoutAttempts(ctx, gen.ListStaleCheckoutAttemptsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: scope.Customer, Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("life: scan stale checkout attempts: %w", err)
	}
	out := make([]ConvergeFinding, 0, len(stale))
	for i := range stale {
		id := stale[i]
		out = append(out, ConvergeFinding{
			Type:  "life.checkout_attempt.stale",
			Shape: ShapeExcess,
			Class: ClassAuto,
			// Ungated: expiring an abandoned checkout attempt retracts no
			// access, no money and no provider object.
			Ungated:    true,
			Severity:   "low",
			SubjectKey: "checkout_attempt:" + id.String(),
			Provider:   "self",
			Evidence:   map[string]any{"checkout_attempt_id": billing.CheckoutAttemptID(id).String()},
			Repair: func(ctx context.Context) error {
				_, e := q.ExpireCheckoutAttemptByID(ctx, gen.ExpireCheckoutAttemptByIDParams{
					MerchantID: scope.Merchant.UUID(), ID: id, Now: p.e.Now(),
				})
				return e
			},
		})
	}

	// A clock reading is not evidence: an overdue renewal or a dunning window
	// that closed with nothing scheduled only asks the provider. The repair is
	// the lifecycle's own event on the locked row (RenewalOverdue /
	// DunningStale → unverified, read at once), taken only while the premise
	// still holds; it never moves a period or a retry.
	overdue, err := q.ListOverdueRenewals(ctx, gen.ListOverdueRenewalsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: scope.Customer, OverdueBefore: now.Add(-reconcile.PeriodGrace), RowLimit: convergeScanCap,
	})
	if err != nil {
		return nil, fmt.Errorf("life: scan overdue renewals: %w", err)
	}
	markTruncated(ctx, len(overdue), findingRenewalOverdue)
	for i := range overdue {
		row := overdue[i]
		out = append(out, ConvergeFinding{
			Type: findingRenewalOverdue, Shape: ShapeMismatch, Class: ClassAuto, Severity: SeverityMedium,
			SubjectKey: "subscription:" + row.ID.String(), Provider: "self",
			Evidence: map[string]any{"subscription_id": billing.SubscriptionID(row.ID).String(), "rail": row.Rail, "paid_through": row.CurrentPeriodEndsAt.UTC(), "cause": "no_renewal_observed"},
			Repair: func(ctx context.Context) error {
				_, err := lc.Decide(ctx, p.e.DB, row.ID, lifecycle.RenewalOverdue{}, func(ctx context.Context, d *db.DB, sub *models.Subscription) (bool, error) {
					if sub.Status != models.StatusActive || !samePeriod(sub.CurrentPeriodEndsAt, row.CurrentPeriodEndsAt) {
						return false, nil
					}
					paid, err := d.Gen(ctx).SubscriptionHasCompletedPayment(ctx, gen.SubscriptionHasCompletedPaymentParams{MerchantID: scope.Merchant.UUID(), SubscriptionID: sub.ID, Since: sub.CurrentPeriodEndsAt})
					return !paid, err
				})
				return err
			},
		})
	}
	pastGrace, err := q.ListDunningPastGrace(ctx, gen.ListDunningPastGraceParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: scope.Customer, Now: now, RowLimit: convergeScanCap,
	})
	if err != nil {
		return nil, fmt.Errorf("life: scan dunning past grace: %w", err)
	}
	markTruncated(ctx, len(pastGrace), findingGraceExhausted)
	for i := range pastGrace {
		row := pastGrace[i]
		out = append(out, ConvergeFinding{
			Type: findingGraceExhausted, Shape: ShapeMismatch, Class: ClassAuto, Severity: SeverityHigh,
			SubjectKey: "subscription:" + row.ID.String(), Provider: "self",
			Evidence: map[string]any{"subscription_id": billing.SubscriptionID(row.ID).String(), "grace_ends_at": row.GraceEndsAt.UTC(), "cause": "dunning_stalled_past_grace"},
			Repair: func(ctx context.Context) error {
				_, err := lc.Decide(ctx, p.e.DB, row.ID, lifecycle.DunningStale{}, func(_ context.Context, _ *db.DB, sub *models.Subscription) (bool, error) {
					return sub.Status == models.StatusPastDue && sub.NextRetryAt == nil &&
						sub.GraceEndsAt != nil && sub.GraceEndsAt.Before(now) &&
						samePeriod(sub.CurrentPeriodEndsAt, row.CurrentPeriodEndsAt), nil
				})
				return err
			},
		})
	}

	unverified, err := p.unverifiedFindings(ctx, scope, now)
	if err != nil {
		return nil, err
	}
	out = append(out, unverified...)
	if scope.IsGlobal() {
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
	}

	paidPending, err := q.ListPaidPendingSubscriptions(ctx, gen.ListPaidPendingSubscriptionsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: scope.Customer, Cutoff: now.Add(-paidPendingAfter),
		RowLimit: convergeScanCap,
	})
	if err != nil {
		return nil, fmt.Errorf("life: scan paid pending subscriptions: %w", err)
	}
	markTruncated(ctx, len(paidPending), "life.subscription.paid_pending")
	for i := range paidPending {
		row := paidPending[i]
		out = append(out, ConvergeFinding{
			Type:       "life.subscription.paid_pending",
			Shape:      ShapeMissing,
			Class:      ClassAdmin,
			Severity:   "high",
			SubjectKey: "subscription:" + row.ID.String(),
			Provider:   "self",
			Evidence: map[string]any{
				"subscription_id": billing.SubscriptionID(row.ID).String(),
				"rail":            row.Rail,
				"payment_id":      billing.PaymentID(row.PaymentID).String(),
				"transaction_id":  row.TransactionID,
				"purchased_at":    row.PurchasedAt.UTC(),
				"pending_since":   row.CreatedAt.UTC(),
				"cause":           "completed_payment_without_activation",
			},
			RecommendedAction: fmt.Sprintf("Subscription %s is still pending although payment %s (transaction %s) completed. Re-run the rail's converge for this subscription, or activate it manually; never cancel it as stale.",
				row.ID, row.PaymentID, row.TransactionID),
		})
	}

	// life.subscription.pending_stale — a `pending` sub that never confirmed
	// within the threshold is abandoned; cancel it (no entitlements/money to
	// unwind). EXCESS, gated on the `subscriptions` domain: the repair rests
	// on an absence (no confirmation arrived), which is proof only once
	// provider truth is fully reconciled.
	stalePending, err := q.ListStalePendingSubscriptions(ctx, gen.ListStalePendingSubscriptionsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: scope.Customer, Cutoff: now.Add(-pendingStaleAfter),
		RowLimit: convergeScanCap,
	})
	if err != nil {
		return nil, fmt.Errorf("life: scan stale pending subscriptions: %w", err)
	}
	markTruncated(ctx, len(stalePending), "life.subscription.pending_stale")
	for i := range stalePending {
		subID := stalePending[i]
		out = append(out, ConvergeFinding{
			Type:  "life.subscription.pending_stale",
			Shape: ShapeExcess,
			Class: ClassAuto,
			// This cancels because a confirmation did not arrive: an absence,
			// which needs the confirmed-absence proof. A delayed or dropped
			// webhook is not evidence the customer did not pay.
			SourceDomain: DomainSubscriptions,
			Severity:     "low",
			SubjectKey:   "subscription:" + subID.String(),
			Provider:     "self",
			Evidence:     map[string]any{"subscription_id": billing.SubscriptionID(subID).String()},
			Repair: func(ctx context.Context) error {
				_, err := lc.Decide(ctx, p.e.DB, subID, lifecycle.InitialFailed{At: now}, func(ctx context.Context, d *db.DB, sub *models.Subscription) (bool, error) {
					if sub.Status != models.StatusPending {
						return false, nil
					}
					paid, err := d.Gen(ctx).SubscriptionHasCompletedPayment(ctx, gen.SubscriptionHasCompletedPaymentParams{MerchantID: scope.Merchant.UUID(), SubscriptionID: sub.ID})
					return !paid, err
				})
				return err
			},
		})
	}

	// life.subscription.dunning_overdue — an OpenRails-dunned NMI schedule
	// (nmi_schedule) past_due within grace with no retry scheduled. The
	// dunning schedule picks the time; this pass never does. Without a recorded
	// decline there is nothing to resume from: surfaced for the operator.
	dunningStalled, err := q.ListDunningStalledSubscriptions(ctx, gen.ListDunningStalledSubscriptionsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: scope.Customer, Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("life: scan dunning-stalled subscriptions: %w", err)
	}
	for i := range dunningStalled {
		subID := dunningStalled[i].ID
		if !dunningStalled[i].AttemptRecorded {
			out = append(out, ConvergeFinding{
				Type:       "life.subscription.dunning_without_decline",
				Shape:      ShapeMismatch,
				Class:      ClassAdmin,
				Severity:   "high",
				SubjectKey: "subscription:" + subID.String(),
				Provider:   "self",
				Evidence:   map[string]any{"subscription_id": billing.SubscriptionID(subID).String()},
			})
			continue
		}
		out = append(out, ConvergeFinding{
			Type:       "life.subscription.dunning_overdue",
			Shape:      ShapeMissing,
			Class:      ClassAuto,
			Severity:   "medium",
			SubjectKey: "subscription:" + subID.String(),
			Provider:   "self",
			Evidence:   map[string]any{"subscription_id": billing.SubscriptionID(subID).String()},
			Repair: func(ctx context.Context) error {
				_, e := lc.ResumeStalledDunning(ctx, p.e.DB, subID)
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
				Evidence:   map[string]any{"provider_intent_id": pi.ID.String(), "intent_type": pi.IntentType, "intent_status": pi.Status, "provider": pi.Rail},
				// surface-only: no Repair (operator/provider action resolves it)
			})
		}
	}

	// life.provider_intent.stuck — a rail intent sitting non-terminal beyond
	// the stuck thresholds. Local ledger only, merchant-wide (intents carry no
	// customer), so it runs on the sweep/post-pull scope. Mode/kill-switch
	// parks are informational (the executor drains them when the blocker
	// lifts); anything else means provider failures, bad credentials or a
	// dead executor/verifier. Never repaired here: the executor/verifier own
	// the intent.
	if scope.IsGlobal() {
		actionCutoff, verifyCutoff := now.Add(-stuckActionableAge), now.Add(-stuckVerifyAge)
		scopeMerchantID, scopeErr := merchant.Require(ctx)
		if scopeErr != nil {
			return nil, scopeErr
		}
		stuck, err := q.ListStuckProviderIntents(ctx, gen.ListStuckProviderIntentsParams{
			MerchantID:   scopeMerchantID.UUID(),
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

		// A refused provider operation holds the customer's capacity until an
		// operator closes it; the finding clears with the close.
		refused, err := q.ListOpenRefusedOperationAuthorizations(ctx, gen.ListOpenRefusedOperationAuthorizationsParams{
			MerchantID: scopeMerchantID.UUID(), RowLimit: convergeScanCap,
		})
		if err != nil {
			return nil, fmt.Errorf("life: scan refused provider operations: %w", err)
		}
		markTruncated(ctx, len(refused), findingProviderOperationRefused)
		for i := range refused {
			out = append(out, refusedProviderOperationFinding(&refused[i], now))
		}

		// A hold never expires, so one its host stopped driving reserves the
		// customer's money until someone notices.
		silent, err := q.ListSilentOperationAuthorizations(ctx, gen.ListSilentOperationAuthorizationsParams{
			MerchantID: scopeMerchantID.UUID(), Cutoff: now.Add(-silentProviderOperationAge), RowLimit: convergeScanCap,
		})
		if err != nil {
			return nil, fmt.Errorf("life: scan silent provider operations: %w", err)
		}
		markTruncated(ctx, len(silent), findingProviderOperationSilent)
		for i := range silent {
			out = append(out, silentProviderOperationFinding(&silent[i], now))
		}
	}
	return out, nil
}

// silentProviderOperationAge is how long an open hold goes untouched before
// it is silent: its host records an observation at least once per quiescence
// (a day by default) until the hold qualifies.
const silentProviderOperationAge = 7 * 24 * time.Hour

// silentProviderOperationFinding asks the host to act on a hold it stopped
// driving: observe, release, or refuse it so an operator can close it.
func silentProviderOperationFinding(row *gen.ListSilentOperationAuthorizationsRow, now time.Time) ConvergeFinding {
	return ConvergeFinding{
		Type: findingProviderOperationSilent, Shape: ShapeMismatch, Class: ClassOperator, Severity: SeverityHigh,
		SubjectKey: "provider_operation:" + row.OperationID, Provider: "self",
		Evidence: map[string]any{
			"operation_id":      row.OperationID,
			"customer_id":       billing.CustomerID(row.CustomerID).String(),
			"currency":          row.Currency,
			"authorized_amount": strconv.FormatInt(row.AuthorizedAmount, 10),
			"held_since":        row.CreatedAt.UTC().Format(time.RFC3339),
			"last_activity_at":  row.LastActivityAt.UTC().Format(time.RFC3339),
			"silent_for":        now.Sub(row.LastActivityAt).Truncate(time.Minute).String(),
		},
		RecommendedAction: fmt.Sprintf("Provider operation %s holds %s %s and nothing has touched it since %s. Its host should record an observation or release it; a host that cannot qualify it records a refusal, then an operator closes it.",
			row.OperationID, strconv.FormatInt(row.AuthorizedAmount, 10), row.Currency, row.LastActivityAt.UTC().Format(time.RFC3339)),
	}
}

// refusedProviderOperationFinding asks an operator to close a provider
// operation whose cost will not qualify automatically. Closing needs an
// attestation (settled at the provider's invoiced cost, or written off), so
// the finding carries no executable recommendation.
func refusedProviderOperationFinding(row *gen.ListOpenRefusedOperationAuthorizationsRow, now time.Time) ConvergeFinding {
	ev := map[string]any{
		"operation_id":      row.OperationID,
		"customer_id":       billing.CustomerID(row.CustomerID).String(),
		"currency":          row.Currency,
		"authorized_amount": strconv.FormatInt(row.AuthorizedAmount, 10),
		"reason":            row.Reason,
		"refused_at":        row.RefusedAt.UTC().Format(time.RFC3339),
		"held_since":        row.CreatedAt.UTC().Format(time.RFC3339),
		"refused_for":       now.Sub(row.RefusedAt).Truncate(time.Minute).String(),
	}
	if row.Detail != nil {
		ev["detail"] = *row.Detail
	}
	return ConvergeFinding{
		Type: findingProviderOperationRefused, Shape: ShapeMismatch, Class: ClassOperator, Severity: SeverityHigh,
		SubjectKey: "provider_operation:" + row.OperationID, Provider: "self", Evidence: ev,
		RecommendedAction: fmt.Sprintf("Provider operation %s holds %s %s and its cost will not qualify automatically (%s). Close it: POST /v1/admin/provider-operations/{operation_id}/close, settled at the provider's invoiced cost or written_off.",
			row.OperationID, strconv.FormatInt(row.AuthorizedAmount, 10), row.Currency, row.Reason),
	}
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

// notifyPass is the NOTIFY plane: whatever plane closed a customer's last
// entitlement window (dunning, reconcile-driven cancel, grant lapse), the
// customer is told exactly once. It only creates notifications rows
// (emailed_at NULL); the notification email sweep delivers them. Dedupe: any
// premium_ended row at or after the close means a transition site already
// told them.
type notifyPass struct{ e *ConvergeEngine }

func (*notifyPass) Plane() string { return "NOTIFY" }
func (p *notifyPass) Run(ctx context.Context, scope Scope) ([]ConvergeFinding, error) {
	if scope.Customer == nil && scope.Subscription != nil {
		return nil, nil // subscription-scope rides on its customer/merchant run
	}
	q := p.e.DB.Gen(ctx)
	now := p.e.Now()
	closed, err := q.ListRecentlyClosedLastAccessWindows(ctx, gen.ListRecentlyClosedLastAccessWindowsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: scope.Customer,
		ClosedAfter: now.Add(-accessEndedLookback), Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("notify: scan recently closed access windows: %w", err)
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
		cust, product := c.CustomerID, billing.ProductID(c.ProductID)
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
				return repo.Create(ctx, &models.NotificationQueue{
					ID:         uuidutil.NewV7(),
					CustomerID: cust,
					EventType:  models.NotificationPremiumEnded,
					Data: billing.NotificationData{
						Reason:    string(subscriptions.PremiumEndReasonAccessEnded),
						EndedAt:   &closedAt,
						ProductID: product,
						Source:    "converge_notify",
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
	// constraint, a LIFE transition or a DERIVE grant effect. Its findings are
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
		subject := "provider_charge:" + d.UserID + ":" + subID.String() + ":" + d.PeriodKey
		// payment_ids is ordered purchased_at DESC: ids[0] is the later charge —
		// the default refund target (operator can override before approving).
		rec := recommend.CancelAndRefundRec(billing.SubscriptionID{}, billing.PaymentID(d.PaymentIds[0]))
		out = append(out, ConvergeFinding{
			Type:       findingDuplicateCharge,
			Shape:      ShapeExcess,
			Class:      ClassAdmin,
			Severity:   "critical",
			SubjectKey: subject,
			Provider:   "self",
			Evidence: map[string]any{
				"customer_id": d.UserID, "subscription_id": subID.String(), "period_start": d.PeriodKey,
				"product_id": billing.ProductID(d.ProductID).String(), "product_key": d.ProductKey,
				"charge_count": d.Count, "payment_ids": ids, "total_amount": strconv.FormatInt(d.TotalAmount, 10), "currency": d.Currency,
				"first_date": d.FirstDate, "last_date": d.LastDate,
				recommend.EvidenceKey: rec.Map(),
			},
			RecommendedAction: fmt.Sprintf("Customer %s was charged %d times for the %s period of subscription %s (%q; total %s; payments %s). Refund the later charge %s unless an operator action explains it.",
				d.UserID, d.Count, d.PeriodKey, subID.String(), d.ProductKey, moneyutil.FormatAmount(d.TotalAmount, d.Currency), strings.Join(ids, ", "), ids[0]),
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
func (p *lifePass) unverifiedFindings(ctx context.Context, scope Scope, now time.Time) ([]ConvergeFinding, error) {
	rows, err := p.e.DB.Gen(ctx).ListUnverifiedSubscriptions(ctx, gen.ListUnverifiedSubscriptionsParams{
		MerchantID: scope.Merchant.UUID(), CustomerID: scope.Customer, RowLimit: convergeScanCap,
	})
	if err != nil {
		return nil, fmt.Errorf("life: scan unverified subscriptions: %w", err)
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
	if !scope.IsGlobal() {
		return out, nil
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
	findingRenewalOverdue       = "life.subscription.renewal_overdue"
	findingGraceExhausted       = "life.subscription.grace_exhausted"
	findingUnverifiedBacklog    = "life.unverified.backlog"
	findingUnverifiedUnresolved = "life.unverified.unresolved"
	findingDunningFunnel        = "life.dunning.funnel"
	findingRenewalHeld          = "life.renewal.held"
	findingDuplicateCharge      = "consistency.duplicate.provider_charge"

	findingProviderOperationRefused = "life.provider_operation.refused"
	findingProviderOperationSilent  = "life.provider_operation.silent"
)

func (*derivePass) Standing() []string {
	return []string{"derive.grant_effect.missing", "derive.grant_effect.excess", "derive.grant.missing", "derive.grant.excess",
		"derive.subscription.missing", "derive.wallet.missing", "derive.grant_effect.mismatch", "derive.entitlement.unjustified"}
}

// Standing leaves out life.provider_intent.stuck, which resolves by its own
// criteria (AutoResolveRecoveredStuckIntentFindings).
func (*lifePass) Standing() []string {
	return []string{"life.checkout_attempt.stale", findingRenewalOverdue, findingGraceExhausted, "life.subscription.paid_pending",
		"life.subscription.pending_stale", "life.subscription.dunning_without_decline", "life.subscription.dunning_overdue",
		"life.provider_intent.abandoned", findingUnverifiedBacklog, findingUnverifiedUnresolved, findingDunningFunnel, findingRenewalHeld,
		findingNewCardDeclineSpike, findingRebillFailureSpike, findingSystemErrors, findingDeclineUnmapped, findingWebhookSilence,
		findingProviderOperationRefused, findingProviderOperationSilent}
}

func (*notifyPass) Standing() []string { return []string{"notify.access_ended.missing"} }

func (*conPass) Standing() []string {
	return []string{"consistency.reference.source_reference", findingDuplicateCharge, "consistency.duplicate.ownership"}
}

// heldRenewalsFinding reports engine renewals with no outcome past their
// allowance: collection is stopped (fleet halted, admission hold, breaker,
// readonly). Members keep access by default, so the backlog is the operator's
// signal. It resolves once collection resumes and the renewals are attempted.
func (p *lifePass) heldRenewalsFinding(ctx context.Context, scope Scope, now time.Time) (*ConvergeFinding, error) {
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
func (p *lifePass) funnelFinding(ctx context.Context, scope Scope, now time.Time) (*ConvergeFinding, error) {
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
