//go:build integration

package subscriptions

// #816 re-driver suite: blocked #813 plan-change rows heal UNATTENDED — the
// deferred far-future pushes as each subscription enters its final
// pre-effective period, and the crash-window rows whose state already matches
// the target — through the same execute paths a manual re-run uses, safely
// alongside one.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

// rollPeriod simulates the subscription's renewal boundary having passed: the
// next period begins. Only the period fields move — exactly what a real
// renewal does to the window predicate's inputs.
func (f *planMigrationFixture) rollPeriod(t *testing.T, ctx context.Context, subID uuid.UUID) {
	t.Helper()
	_, err := f.pool.Exec(ctx, `
		UPDATE openrails.subscriptions
		SET current_period_starts_at = current_period_ends_at,
		    current_period_ends_at   = current_period_ends_at + interval '30 days'
		WHERE id = $1`, subID)
	require.NoError(t, err)
}

func (f *planMigrationFixture) repriceRow(t *testing.T, ctx context.Context, subID uuid.UUID) (id uuid.UUID, status, reason string) {
	t.Helper()
	require.NoError(t, f.pool.QueryRow(ctx, `
		SELECT id, status, COALESCE(blocked_reason, '') FROM openrails.subscription_reprices
		WHERE subscription_id = $1 ORDER BY created_at DESC LIMIT 1`, subID).Scan(&id, &status, &reason))
	return id, status, reason
}

func (f *planMigrationFixture) periodEnd(t *testing.T, ctx context.Context, subID uuid.UUID) time.Time {
	t.Helper()
	var end time.Time
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT current_period_ends_at FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&end))
	return end
}

func schedulesFor(f *fakeStripePusher, railSubID string) int {
	n := 0
	for _, s := range f.schedules {
		if s["subscription_id"] == railSubID {
			n++
		}
	}
	return n
}

func pushesFor(f *fakeNMIPusher, railSubID string) int {
	n := 0
	for _, p := range f.pushes {
		if p["rail_subscription_id"] == railSubID {
			n++
		}
	}
	return n
}

// TestPlanMigrationRedrive_StripeDeferredHealsAtRollover: the headline #816
// mechanic — a far-future migration blocks at commit, then heals UNATTENDED
// once the subscription enters its final pre-effective period, with the
// rebill date untouched.
func TestPlanMigrationRedrive_StripeDeferredHealsAtRollover(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, railSubID := f.createSubscriptionOnRail(t, ctx, f.stripeSourcePriceID, "stripe", false)

	// Effective beyond the current period (period end = now+30d) — the commit
	// blocks the push honestly.
	effective := f.clock.Now().Add(45 * 24 * time.Hour)
	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{
		SourcePriceID: f.stripeSourcePriceID, TargetPriceID: f.stripeTargetPriceID, EffectiveAt: effective,
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.Blocked)
	rowID, status, reason := f.repriceRow(t, ctx, subID)
	require.Equal(t, "blocked", status)
	require.Contains(t, reason, "stripe_deferred_push_required")
	require.Zero(t, schedulesFor(f.stripe, railSubID))

	// Not yet in window: the re-driver defers, touches nothing.
	rd, err := f.pm.RedriveBlocked(ctx, 500)
	require.NoError(t, err)
	require.GreaterOrEqual(t, rd.Deferred, 1)
	_, status, _ = f.repriceRow(t, ctx, subID)
	require.Equal(t, "blocked", status)
	require.Zero(t, schedulesFor(f.stripe, railSubID))

	// The renewal boundary passes; the sub is now in its final pre-effective
	// period (new period end = now+60d >= effective now+45d).
	f.rollPeriod(t, ctx, subID)
	endBefore := f.periodEnd(t, ctx, subID)

	rd, err = f.pm.RedriveBlocked(ctx, 500)
	require.NoError(t, err)
	require.GreaterOrEqual(t, rd.Redriven, 1)

	// Push fired exactly once; the row is scheduled again (stripe's converge
	// applies it at the boundary); the rebill date never moved.
	require.Equal(t, 1, schedulesFor(f.stripe, railSubID))
	gotID, status, _ := f.repriceRow(t, ctx, subID)
	require.Equal(t, rowID, gotID)
	require.Equal(t, "scheduled", status)
	require.Equal(t, endBefore, f.periodEnd(t, ctx, subID))

	// Idempotent: a second tick finds nothing to do for this row.
	_, err = f.pm.RedriveBlocked(ctx, 500)
	require.NoError(t, err)
	require.Equal(t, 1, schedulesFor(f.stripe, railSubID))
	_, status, _ = f.repriceRow(t, ctx, subID)
	require.Equal(t, "scheduled", status)
}

// TestPlanMigrationRedrive_NMIDeferredHealsAtRollover: same heal on the
// gateway-native NMI rail — the push carries the internal cutover, so the row
// lands applied and the subscription is on the target.
func TestPlanMigrationRedrive_NMIDeferredHealsAtRollover(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, railSubID := f.createSubscriptionOnRail(t, ctx, f.lowPriceID, "nmi", false)

	effective := f.clock.Now().Add(45 * 24 * time.Hour)
	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{
		SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID, EffectiveAt: effective,
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.Blocked)
	_, status, reason := f.repriceRow(t, ctx, subID)
	require.Equal(t, "blocked", status)
	require.Contains(t, reason, "nmi_deferred_push_required")
	require.Zero(t, pushesFor(f.nmi, railSubID))

	f.rollPeriod(t, ctx, subID)
	endBefore := f.periodEnd(t, ctx, subID)

	rd, err := f.pm.RedriveBlocked(ctx, 500)
	require.NoError(t, err)
	require.GreaterOrEqual(t, rd.Redriven, 1)

	require.Equal(t, 1, pushesFor(f.nmi, railSubID))
	_, status, _ = f.repriceRow(t, ctx, subID)
	require.Equal(t, "applied", status)
	priceID, productID := f.subscriptionRow(t, ctx, subID)
	require.Equal(t, f.targetPriceID, priceID)
	require.Equal(t, f.targetProductID, productID)
	require.Equal(t, endBefore, f.periodEnd(t, ctx, subID))
}

// TestPlanMigrationRedrive_CrashWindowConvergesWithoutPush: a row blocked
// after its rail push succeeded (the sub already carries the target) heals to
// applied with ZERO rail traffic.
func TestPlanMigrationRedrive_CrashWindowConvergesWithoutPush(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, railSubID := f.createSubscriptionOnRail(t, ctx, f.lowPriceID, "nmi", false)

	// Manufacture the crash window: push verified + sub cut over, then the row
	// transition died — sub on target, row blocked with the push-failure
	// prefix.
	_, err := f.pool.Exec(ctx, `
		UPDATE openrails.subscriptions SET price_id = $2, product_id = $3 WHERE id = $1`,
		subID, f.targetPriceID, f.targetProductID)
	require.NoError(t, err)
	row, err := f.repriceRepo.CreateBlockedReprice(ctx, subID, f.lowPriceID, f.targetPriceID,
		f.clock.Now(), nil, models.RepriceKindPlanChange, "rail_push_failed: apply immediately: db went away")
	require.NoError(t, err)

	rd, err := f.pm.RedriveBlocked(ctx, 500)
	require.NoError(t, err)
	require.GreaterOrEqual(t, rd.Converged, 1)

	got, err := f.repriceRepo.GetByID(ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, models.RepriceStatusApplied, got.Status)
	require.Zero(t, pushesFor(f.nmi, railSubID))
	require.Zero(t, schedulesFor(f.stripe, railSubID))
}

// TestPlanMigrationRedrive_ConcurrentManualRerunPushesOnce: a manual re-run
// that got there first owns the subscription; the re-driver skips the stale
// blocked row — exactly one rail push total.
func TestPlanMigrationRedrive_ConcurrentManualRerunPushesOnce(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, railSubID := f.createSubscriptionOnRail(t, ctx, f.stripeSourcePriceID, "stripe", false)

	effective := f.clock.Now().Add(45 * 24 * time.Hour)
	_, err := f.pm.Migrate(ctx, PlanMigrationRequest{
		SourcePriceID: f.stripeSourcePriceID, TargetPriceID: f.stripeTargetPriceID, EffectiveAt: effective,
	})
	require.NoError(t, err)
	blockedID, status, _ := f.repriceRow(t, ctx, subID)
	require.Equal(t, "blocked", status)

	f.rollPeriod(t, ctx, subID)

	// The operator re-runs manually first: a fresh row schedules and pushes.
	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{
		SourcePriceID: f.stripeSourcePriceID, TargetPriceID: f.stripeTargetPriceID, EffectiveAt: effective,
		ArchiveSource: func() *bool { b := false; return &b }(),
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.Scheduled)
	require.Equal(t, 1, schedulesFor(f.stripe, railSubID))

	// The re-driver then meets the old blocked row: the one-scheduled index +
	// conflict check make it skip — no second push, old row stays blocked.
	_, err = f.pm.RedriveBlocked(ctx, 500)
	require.NoError(t, err)
	require.Equal(t, 1, schedulesFor(f.stripe, railSubID))
	var oldStatus string
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT status FROM openrails.subscription_reprices WHERE id = $1`, blockedID).Scan(&oldStatus))
	require.Equal(t, "blocked", oldStatus)
}

// TestPlanMigrationRedrive_TransientPushFailureRetriesAndReblocks: a
// transient rail failure re-drives on the next tick; a persistent one
// re-blocks with the fresh reason and keeps the ledger honest.
func TestPlanMigrationRedrive_TransientPushFailureRetriesAndReblocks(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, railSubID := f.createSubscriptionOnRail(t, ctx, f.stripeSourcePriceID, "stripe", false)

	// First commit fails at the rail (transient 500): row degrades to blocked.
	f.stripe.scheduleErr = fmt.Errorf("stripe 500")
	_, err := f.pm.Migrate(ctx, PlanMigrationRequest{
		SourcePriceID: f.stripeSourcePriceID, TargetPriceID: f.stripeTargetPriceID,
	})
	require.NoError(t, err)
	rowID, status, reason := f.repriceRow(t, ctx, subID)
	require.Equal(t, "blocked", status)
	require.Contains(t, reason, "rail_push_failed")

	// Still failing: the re-driver re-blocks (fresh reason), row keeps its id.
	rd, err := f.pm.RedriveBlocked(ctx, 500)
	require.NoError(t, err)
	require.GreaterOrEqual(t, rd.Failed, 1)
	gotID, status, reason := f.repriceRow(t, ctx, subID)
	require.Equal(t, rowID, gotID)
	require.Equal(t, "blocked", status)
	require.Contains(t, reason, "stripe 500")

	// Rail heals: the next tick migrates it.
	f.stripe.scheduleErr = nil
	rd, err = f.pm.RedriveBlocked(ctx, 500)
	require.NoError(t, err)
	require.GreaterOrEqual(t, rd.Redriven, 1)
	require.Equal(t, 1, schedulesFor(f.stripe, railSubID))
	_, status, _ = f.repriceRow(t, ctx, subID)
	require.Equal(t, "scheduled", status)
}

// TestPlanMigrationRedrive_BatchHeaderStaysInSync: after the re-driver moves
// rows between blocked and scheduled, the batch header agrees with its rows.
func TestPlanMigrationRedrive_BatchHeaderStaysInSync(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, _ := f.createSubscriptionOnRail(t, ctx, f.stripeSourcePriceID, "stripe", false)

	effective := f.clock.Now().Add(45 * 24 * time.Hour)
	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{
		SourcePriceID: f.stripeSourcePriceID, TargetPriceID: f.stripeTargetPriceID, EffectiveAt: effective,
	})
	require.NoError(t, err)
	require.NotNil(t, res.BatchID)
	batch, _, err := f.pm.GetBatch(ctx, *res.BatchID, 100, 0)
	require.NoError(t, err)
	require.Equal(t, 0, batch.SubscriptionsScheduled)
	require.Equal(t, 1, batch.SubscriptionsBlocked)

	f.rollPeriod(t, ctx, subID)
	_, err = f.pm.RedriveBlocked(ctx, 500)
	require.NoError(t, err)

	batch, rows, err := f.pm.GetBatch(ctx, *res.BatchID, 100, 0)
	require.NoError(t, err)
	require.Equal(t, 1, batch.SubscriptionsScheduled)
	require.Equal(t, 0, batch.SubscriptionsBlocked)
	require.Len(t, rows, 1)
	require.Equal(t, models.RepriceStatusScheduled, rows[0].Status)
}

// TestPlanMigrationRedrive_TerminalClassificationsUntouched: classification
// blocks (rail_requires_user_action) are not push failures and never
// re-drive.
func TestPlanMigrationRedrive_TerminalClassificationsUntouched(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, _ := f.createSubscriptionOnRail(t, ctx, f.lowPriceID, "ccbill", false)

	_, err := f.pm.Migrate(ctx, PlanMigrationRequest{
		SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID,
	})
	require.NoError(t, err)
	rowID, status, reason := f.repriceRow(t, ctx, subID)
	require.Equal(t, "blocked", status)
	require.Equal(t, "rail_requires_user_action", reason)

	_, err = f.pm.RedriveBlocked(ctx, 500)
	require.NoError(t, err)
	gotID, status, reason := f.repriceRow(t, ctx, subID)
	require.Equal(t, rowID, gotID)
	require.Equal(t, "blocked", status)
	require.Equal(t, "rail_requires_user_action", reason)
}

// TestPlanMigrationRedrive_CancelledSubscriptionLeftBlocked: a subscription
// that left the migratable cohort is never touched.
func TestPlanMigrationRedrive_CancelledSubscriptionLeftBlocked(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, railSubID := f.createSubscriptionOnRail(t, ctx, f.stripeSourcePriceID, "stripe", false)

	effective := f.clock.Now().Add(45 * 24 * time.Hour)
	_, err := f.pm.Migrate(ctx, PlanMigrationRequest{
		SourcePriceID: f.stripeSourcePriceID, TargetPriceID: f.stripeTargetPriceID, EffectiveAt: effective,
	})
	require.NoError(t, err)

	_, err = f.pool.Exec(ctx, `
		UPDATE openrails.subscriptions
		SET status = 'cancelled', cancelled_at = now(), cancel_type = 'user_cancelled'
		WHERE id = $1`, subID)
	require.NoError(t, err)
	f.rollPeriod(t, ctx, subID)

	_, err = f.pm.RedriveBlocked(ctx, 500)
	require.NoError(t, err)
	_, status, _ := f.repriceRow(t, ctx, subID)
	require.Equal(t, "blocked", status)
	require.Zero(t, schedulesFor(f.stripe, railSubID))
}
