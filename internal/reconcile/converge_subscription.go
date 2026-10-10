package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// A verified webhook is only a wake-up signal: the coalesced fetch job probes
// provider truth for one subscription and converges it through the same
// snapshot → Decide → side effects → ApplyDecision pipeline the unknown-cohort
// resolution uses.

// SubscriptionConvergence reports one converge pass.
type SubscriptionConvergence struct {
	Decision   Decision
	Applied    bool
	Backfilled int
}

// SubscriptionStateOf maps a local row onto the decider's view of it.
func SubscriptionStateOf(sub *models.Subscription) SubscriptionState {
	return SubscriptionState{
		CollectionPolicy:   sub.CollectionPolicy,
		Status:             string(sub.Status),
		Rail:               string(sub.Rail),
		RailSubscriptionID: sub.RailSubscriptionID,
		PeriodStart:        sub.CurrentPeriodStartsAt,
		PeriodEnd:          sub.CurrentPeriodEndsAt,
		GraceEndsAt:        sub.GraceEndsAt,
		NextRetryScheduled: sub.NextRetryAt != nil,
	}
}

// ConvergeSubscriptionFromSnapshot converges one local subscription to fetched
// provider truth: Decide, backfill the provider's charges (idempotent by
// transaction id), materialize the provider customer id, then ApplyDecision.
// Repeat calls against the same truth are no-ops. Must run merchant-scoped.
// Terminal/pending rows take no transition, but their charges are still
// backfilled: money truth is not lifecycle truth.
func ConvergeSubscriptionFromSnapshot(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, sub *models.Subscription, snap *RemoteSnapshot, now time.Time, dunningWindow time.Duration) (SubscriptionConvergence, error) {
	var floor time.Time
	if database != nil && sub != nil {
		floor = EvidenceFloorFor(ctx, database, sub.MerchantID)
	}
	return convergeSubscriptionFromSnapshotLookback(ctx, database, lc, sub, snap, now, dunningWindow, defaultBackfillLookback, floor)
}

// convergeSubscriptionFromSnapshotLookback is the core with a backfill
// lookback: live planes cap it; the declared import unbounds it (a legacy
// book's charges are all in scope by declaration). floor is the evidence
// staleness floor: the merchant's first-pull instant live, zero for the
// declared import, whose AsOf declaration is the observation and becomes the
// floor (see EvidenceBundle.EvidenceFloor).
func convergeSubscriptionFromSnapshotLookback(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, sub *models.Subscription, snap *RemoteSnapshot, now time.Time, dunningWindow time.Duration, lookback time.Duration, floor time.Time) (SubscriptionConvergence, error) {
	out := SubscriptionConvergence{}
	if database == nil || lc == nil || sub == nil || snap == nil {
		return out, fmt.Errorf("converge subscription: db, lifecycle, subscription and snapshot are required")
	}
	if dunningWindow <= 0 {
		dunningWindow = DefaultDunningWindow
	}

	d := Decide(SubscriptionStateOf(sub), EvidenceBundle{Snapshot: snap, EvidenceFloor: floor}, now, dunningWindow)
	d.Declared = snap.Provider == ProviderDeclared
	if d.EvidenceFloored {
		recordEvidenceStaleFinding(ctx, database.Gen(ctx), billing.MerchantID(sub.MerchantID), snap.Provider, sub.ID.String(), d.Reason)
	}
	// Money truth is mirrored UNCONDITIONALLY: every fetched charge event for
	// this subscription is imported idempotently (by transaction id), even when
	// the decider refuses a transition (terminal/pending rows, early renewals,
	// mid-cycle upgrade invoices outside the decider's boundary window). The
	// lookback cap in backfillSubscriptionPayments bounds age.
	d.Backfill = snapshotChargesFor(sub, snap)
	out.Decision = d

	backfilled, _, err := applyDecisionSideEffects(ctx, database, sub, d, now, lookback)
	if err != nil {
		return out, err
	}
	out.Backfilled = backfilled

	applied, err := ApplyDecision(ctx, database, lc, sub, d, now)
	if err != nil {
		return out, err
	}
	if !applied && d.Kind != TransitionNone {
		// Another writer moved the row (a park, a decline) after it was read:
		// decide once more on the current row from the same provider truth.
		cur, err := subscriptions.NewSubscriptionRepo(database).GetByID(ctx, sub.ID)
		if err != nil {
			return out, fmt.Errorf("converge subscription: reload %s: %w", sub.ID, err)
		}
		if d.stale(cur) {
			*sub = *cur
			d = Decide(SubscriptionStateOf(sub), EvidenceBundle{Snapshot: snap, EvidenceFloor: floor}, now, dunningWindow)
			d.Declared, d.Backfill = snap.Provider == ProviderDeclared, snapshotChargesFor(sub, snap)
			out.Decision = d
			if applied, err = ApplyDecision(ctx, database, lc, sub, d, now); err != nil {
				return out, err
			}
		}
	}
	out.Applied = applied
	if applied && d.Reason == reasonRenewedBeforeDecline {
		// The renewed row now ends where the later decline's period begins.
		next := Decide(SubscriptionStateOf(sub), EvidenceBundle{Snapshot: snap, EvidenceFloor: floor}, now, dunningWindow)
		next.Declared = d.Declared
		if next.Kind == TransitionPastDue {
			if _, err := ApplyDecision(ctx, database, lc, sub, next, now); err != nil {
				return out, err
			}
			out.Decision = next
		}
	}
	return out, nil
}

// defaultBackfillLookback bounds how old a provider charge may be and still be
// imported.
const defaultBackfillLookback = 3 * 365 * 24 * time.Hour

// snapshotChargesFor returns ALL of the snapshot's charge events for one
// subscription (successes and declines) — the money-truth mirror set.
func snapshotChargesFor(sub *models.Subscription, snap *RemoteSnapshot) []RemoteTransaction {
	if sub.RailSubscriptionID == "" {
		return nil
	}
	var txns []RemoteTransaction
	for i := range snap.Transactions {
		if snap.Transactions[i].SubscriptionID == sub.RailSubscriptionID {
			txns = append(txns, snap.Transactions[i])
		}
	}
	return txns
}

// applyDecisionSideEffects lands a decision's evidence-derived side data:
// payment backfill and rail-customer materialization. Returns the backfilled
// payment count and whether a rail customer row was materialized.
func applyDecisionSideEffects(ctx context.Context, database *db.DB, sub *models.Subscription, d Decision, now time.Time, lookbackCap time.Duration) (int, bool, error) {
	q := database.Gen(ctx)
	backfilled, err := backfillSubscriptionPayments(ctx, q, sub, d.Backfill, now, lookbackCap)
	if err != nil {
		return 0, false, fmt.Errorf("converge: backfill %s: %w", sub.ID, err)
	}
	if !d.Declared {
		if err := recordScheduleAttempts(ctx, q, sub, d.Backfill, now); err != nil {
			return backfilled, false, fmt.Errorf("converge: record schedule attempts %s: %w", sub.ID, err)
		}
		if err := refundChargesAfterCancel(ctx, database, sub, d.Backfill, now); err != nil {
			return backfilled, false, fmt.Errorf("converge: charges after cancel %s: %w", sub.ID, err)
		}
	}

	railCustomer := false
	if d.RemoteCustomerID != "" && rails.HasRemoteCustomer(sub.Rail) {
		if err := q.UpsertPSPCustomer(ctx, gen.UpsertPSPCustomerParams{
			CustomerID: sub.CustomerID,
			// The mapping belongs to the account that owns the subscription,
			// whose remote customer id this is.
			PspID:             sub.PspID,
			RemoteCustomerRef: d.RemoteCustomerID,
			At:                now,
			MerchantID:        sub.MerchantID,
		}); err != nil {
			return backfilled, false, fmt.Errorf("converge: materialize rail_customer %s: %w", sub.ID, err)
		}
		railCustomer = true
	}
	return backfilled, railCustomer, nil
}

// refundChargesAfterCancel queues the refund of each mirrored provider charge
// that landed after OpenRails canceled sub for good (a provider stop that has
// not taken effect). Idempotent per charge.
func refundChargesAfterCancel(ctx context.Context, database *db.DB, sub *models.Subscription, txns []RemoteTransaction, now time.Time) error {
	cancelType := ""
	if sub.CancelType != nil {
		cancelType = string(*sub.CancelType)
	}
	for _, t := range txns {
		if !t.Success || !chargeAttempt(t.Type) || t.TransactionID == "" || !chargedAfterCancel(string(sub.Status), cancelType, sub.CanceledAt, t.OccurredAt) {
			continue
		}
		err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			d := database.NewWithPgxTx(tx)
			clock := clockwork.NewFakeClockAt(now)
			payment, err := payments.NewPaymentService(d, clock).GetByPSPTransactionID(ctx, sub.Rail, t.TransactionID)
			if db.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			return intents.RefundChargeAfterCancel(ctx, d, payment, clock)
		})
		if err != nil {
			return err
		}
	}
	return nil
}
