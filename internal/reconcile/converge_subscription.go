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

// #684: the webhook fetch-and-converge entry point. A verified webhook is only
// a wake-up signal — the coalesced fetch job probes provider truth for ONE
// subscription and converges the local row through the #665 decider. This is
// the same (snapshot → Decide → side-effects → ApplyDecision) pipeline the
// unknown-cohort resolution uses, exposed per subscription.

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
// provider truth: Decide over the snapshot evidence, backfill the provider's
// charge records (money truth — idempotent by transaction id), materialize the
// provider customer id, then ApplyDecision through the lifecycle chokepoints.
// Ordering-free by construction: N calls against the same truth are no-ops
// after the first. Must run on a merchant-scoped connection.
//
// Terminal/pending rows take no lifecycle transition (the decider refuses
// them), but their snapshot charges are STILL backfilled: a renewal charge
// against a terminal row is money truth and must leave a durable payment row
// (money truth ≠ lifecycle truth).
func ConvergeSubscriptionFromSnapshot(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, sub *models.Subscription, snap *RemoteSnapshot, now time.Time, dunningWindow time.Duration) (SubscriptionConvergence, error) {
	var floor time.Time
	if database != nil && sub != nil {
		floor = EvidenceFloorFor(ctx, database, sub.MerchantID)
	}
	return convergeSubscriptionFromSnapshotLookback(ctx, database, lc, sub, snap, now, dunningWindow, defaultBackfillLookback, floor)
}

// convergeSubscriptionFromSnapshotLookback is the lookback-parameterized core:
// live planes cap backfill at #634's 3y; the declared import (#737) unbounds it
// (a legacy book's charges are all in scope by declaration).
//
// floor is the #835 evidence-staleness floor. The live path passes the
// merchant's first-pull instant; the declared import passes ZERO on purpose —
// its snapshot is dated at the operator's AsOf horizon and that declaration IS
// the observation, so AsOf becomes the floor (see EvidenceBundle.EvidenceFloor).
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
// imported (#634's 3y recoverable scope).
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
// payment backfill (#634) and rail-customer materialization (#635). Shared by
// the unknown-cohort resolution and the webhook converge path. Returns the
// backfilled-payment count and whether a rail customer row was materialized.
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
			// or#893: the mapping belongs to the account that owns the
			// subscription, which is the account whose remote customer id this
			// is. The subscription's own provenance answers it — no resolution.
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
