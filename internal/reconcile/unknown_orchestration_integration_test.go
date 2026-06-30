//go:build integration

package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// (fakeFetcher is defined in engine_test.go.)

// #633/#634 end-to-end with a FAKE windowed fetcher (fixture provider truth):
// the `unknown` cohort is resolved against one bulk snapshot per rail — renewed /
// past_due / cancelled — missing charges are backfilled (declines as failed), and
// a rail whose fetch fails leaves its subs `unknown` (RailErrors), proving the
// backoff hand-off. One provider call per rail, never per subscription.
func TestReconcileUnknownCohort_FixtureSnapshot(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	lc := subscriptions.NewSubscriptionLifecycleService(appDB, nil, nil, nil, nil, nil, nil, clockwork.NewRealClock())
	now := time.Now().UTC()
	periodEnd := now.Add(-5 * 24 * time.Hour) // lapsed 5d ago (within dunning window)
	start := now.Add(-35 * 24 * time.Hour)
	nextEnd := now.Add(25 * 24 * time.Hour)
	sfx := uuid.NewString()[:8]

	// rail_subscription_ids drive the match.
	rsRenew, rsCancel, rsDecline, rsNMI := "rs-renew-"+sfx, "rs-cancel-"+sfx, "rs-decline-"+sfx, "rs-nmi-"+sfx
	subRenew, subCancel, subDecline, subNMI := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	prices := map[uuid.UUID]uuid.UUID{}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		// Distinct customer + product + price per sub (uq_subscriptions_customer_product_lifecycle).
		mk := func(id uuid.UUID, rail, railSub string) {
			cust := dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
			prod, price := uuid.New(), uuid.New()
			prices[id] = price
			exec(`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id) VALUES ($1,$2,$2,'{}'::jsonb,$3)`, prod, "ur-"+railSub, merchantID)
			exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,merchant_id) VALUES ($1,$2,5000000,'usd',$3)`, price, prod, merchantID)
			exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,started_at,current_period_starts_at,current_period_ends_at)
			      VALUES ($1,$2,$3,$4,$5,'unknown',$6,$7,$8,$8,$9)`, id, merchantID, cust, prod, price, rail, railSub, start, periodEnd)
		}
		mk(subRenew, "ccbill", rsRenew)
		mk(subCancel, "ccbill", rsCancel)
		mk(subDecline, "ccbill", rsDecline)
		mk(subNMI, "nmi", rsNMI)
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			for id, price := range prices {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE subscription_id=$1`, id)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, id)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, price)
			}
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE key LIKE 'ur-rs-%'||$1`, sfx)
			return nil
		})
	})

	ccbillSnap := &RemoteSnapshot{
		Provider: ProviderCCBill,
		Subscriptions: []RemoteSubscription{
			{RailSubscriptionID: rsRenew, Status: SubscriptionStatusActive, NextBillingAt: &nextEnd},
			{RailSubscriptionID: rsCancel, Status: SubscriptionStatusCancelled},
		},
		Transactions: []RemoteTransaction{
			{TransactionID: "tx-renew-" + sfx, SubscriptionID: rsRenew, Type: TransactionTypeSale, Success: true, AmountCents: 5000, Currency: "usd", OccurredAt: periodEnd.Add(time.Hour)},
			{TransactionID: "tx-decline-" + sfx, SubscriptionID: rsDecline, Type: TransactionTypeDecline, Success: false, OccurredAt: periodEnd.Add(time.Hour)},
		},
	}
	fetchers := map[Provider]RailFetcher{
		ProviderCCBill: &fakeFetcher{provider: ProviderCCBill, snap: ccbillSnap},
		ProviderNMI:    &fakeFetcher{provider: ProviderNMI, err: errors.New("nmi unreachable")}, // backoff path
	}

	var res UnknownReconcileResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var err error
		res, err = ReconcileUnknownCohort(ctx, appDB, lc, fetchers, dbtest.TestMerchantID, now, UnknownReconcileOptions{})
		return err
	}))

	// Backoff: the NMI fetch failed → its sub stays unknown + a RailError recorded.
	require.Contains(t, res.RailErrors, ProviderNMI)
	require.GreaterOrEqual(t, res.StillUnknown, 1)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		status := func(id uuid.UUID) string {
			var s string
			require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT status FROM openrails.subscriptions WHERE id=$1`, id).Scan(&s))
			return s
		}
		payCount := func(id uuid.UUID, st string) int {
			var n int
			require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.payments WHERE subscription_id=$1 AND status=$2`, id, st).Scan(&n))
			return n
		}
		require.Equal(t, "active", status(subRenew), "successful renewal → active")
		require.Equal(t, 1, payCount(subRenew, "completed"), "renewal charge backfilled as completed")
		require.Equal(t, "cancelled", status(subCancel), "roster cancelled → cancelled")
		require.Equal(t, "past_due", status(subDecline), "declined within window → past_due")
		require.Equal(t, 1, payCount(subDecline, "failed"), "declined charge backfilled as failed")
		require.Equal(t, "unknown", status(subNMI), "unreachable rail → stays unknown")
		return nil
	}))

	require.Equal(t, 1, res.Renewed)
	require.Equal(t, 1, res.PastDue)
	require.Equal(t, 1, res.Cancelled)
	require.Equal(t, 2, res.Backfilled)
}
