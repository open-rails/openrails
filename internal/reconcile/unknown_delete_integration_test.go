//go:build integration

package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #679 queue-always on the unknown-resolution path: a stale-decline cancel
// (remote NOT confirmed gone) durably queues the deferred NMI delete
// (DeletionScheduledAt marker + nmi_delete intent); a roster-confirmed-gone
// cancel queues nothing — there is nothing left to delete.
func TestReconcileUnknownCohort_StaleDeclineQueuesDeferredDelete(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	lc := subscriptions.NewSubscriptionLifecycleService(appDB, nil, nil, nil, nil, nil, nil, clockwork.NewRealClock())
	// The REAL production scheduler (intent-ledger backed), as wired by
	// jobs_provider_refresh.runUnknownReconcile.
	lc.SetDeferredDeleteScheduler(intents.NewNMIDeleteScheduler(appDB, nil, intents.OriginSystem, "unknown-resolution stale-decline cancel"))

	now := time.Now().UTC()
	periodEnd := now.Add(-30 * 24 * time.Hour) // lapsed far beyond the 14d dunning window
	start := periodEnd.Add(-30 * 24 * time.Hour)
	sfx := uuid.NewString()[:8]

	rsStale, rsGone := "rs-stale-"+sfx, "rs-gone-"+sfx
	subStale, subGone := uuid.New(), uuid.New()
	prices := map[uuid.UUID]uuid.UUID{}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		mk := func(id uuid.UUID, railSub string) {
			cust := dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
			prod, price := uuid.New(), uuid.New()
			prices[id] = price
			exec(`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id) VALUES ($1,$2,$2,'{}'::jsonb,$3)`, prod, "ud-"+railSub, merchantID)
			exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,merchant_id) VALUES ($1,$2,5000000,'usd',$3)`, price, prod, merchantID)
			exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,started_at,current_period_starts_at,current_period_ends_at)
			      VALUES ($1,$2,$3,$4,$5,'unknown','nmi',$6,$7,$7,$8)`, id, merchantID, cust, prod, price, railSub, start, periodEnd)
		}
		mk(subStale, rsStale)
		mk(subGone, rsGone)
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			for id, price := range prices {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.rail_intents WHERE subscription_id=$1`, id)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE subscription_id=$1`, id)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, id)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, price)
			}
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE key LIKE 'ud-rs-%'||$1`, sfx)
			return nil
		})
	})

	// NMI snapshot: rsGone is roster-confirmed cancelled; rsStale is absent from
	// a NON-exhaustive roster but has a stale declined renewal → terminal cancel
	// with the remote possibly still alive and retrying.
	nmiSnap := &RemoteSnapshot{
		Provider: ProviderNMI,
		Subscriptions: []RemoteSubscription{
			{RailSubscriptionID: rsGone, Status: SubscriptionStatusCancelled},
		},
		Transactions: []RemoteTransaction{
			{TransactionID: "tx-stale-" + sfx, SubscriptionID: rsStale, Type: TransactionTypeDecline, Success: false, AmountCents: 5000, Currency: "usd", OccurredAt: periodEnd.Add(time.Hour)},
		},
	}
	fetchers := map[Provider]RailFetcher{
		ProviderNMI: &fakeFetcher{provider: ProviderNMI, snap: nmiSnap},
	}

	var res UnknownReconcileResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var err error
		res, err = ReconcileUnknownCohort(ctx, appDB, lc, fetchers, nil, dbtest.TestMerchantID, now, UnknownReconcileOptions{})
		return err
	}))
	require.Equal(t, 2, res.Cancelled)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var status string
		var marker *time.Time
		var feedback *string

		// Stale decline: cancelled + marker + pending system-origin nmi_delete intent.
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status, deletion_scheduled_at, cancel_feedback FROM openrails.subscriptions WHERE id=$1`, subStale).
			Scan(&status, &marker, &feedback))
		require.Equal(t, "cancelled", status)
		require.NotNil(t, marker, "stale-decline cancel must record DeletionScheduledAt (#679)")
		require.NotNil(t, feedback)
		require.Contains(t, *feedback, "declined beyond dunning window")

		var intentStatus, origin, intentType string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status, origin, intent_type FROM openrails.rail_intents WHERE subscription_id=$1`, subStale).
			Scan(&intentStatus, &origin, &intentType))
		require.Equal(t, "pending", intentStatus)
		require.Equal(t, "system", origin)
		require.Equal(t, intents.TypeNMIDeleteSubscription, intentType)

		// Roster-confirmed gone: cancelled, NO marker, NO intent.
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status, deletion_scheduled_at FROM openrails.subscriptions WHERE id=$1`, subGone).
			Scan(&status, &marker))
		require.Equal(t, "cancelled", status)
		require.Nil(t, marker, "roster-gone cancel must not schedule a delete")
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.rail_intents WHERE subscription_id=$1`, subGone).Scan(&n))
		require.Zero(t, n, "nothing to delete for a roster-confirmed-gone subscription")
		return nil
	}))
}
