//go:build integration

package converge

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #691 fail-open: an auto-renew sub's STANDING entitlement window (end_at NULL)
// survives the whole silence pipeline — period end passes, the REAL converge
// sweep parks the sub `unknown`, repeated sweeps run — access never moves. A
// provider-pull renewal resolution then advances paid-through with access
// continuous. Regression fence for #664 too: convergence never cancels.
func TestConverge_FailOpen_StandingWindowSurvivesParking(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	sfx := uuid.NewString()[:8]

	prod, price, sub, grantID, entID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	feat := "fo-feat-" + sfx
	var cust uuid.UUID
	start := time.Now().UTC().Add(-130 * 24 * time.Hour)
	elapsed := time.Now().UTC().Add(-100 * 24 * time.Hour) // long past any grace

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		cust = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id) VALUES ($1,$2,$2,$3,$4)`,
			prod, "fo-prod-"+sfx, []byte(`{"`+feat+`": null}`), merchantID)
		exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,access_duration_hours,auto_renew,merchant_id) VALUES ($1,$2,5000000,'usd',720,true,$3)`,
			price, prod, merchantID)
		// ccbill: provider-auto-billed, no local vault — the pure silence shape.
		exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,started_at,current_period_starts_at,current_period_ends_at)
		      VALUES ($1,$2,$3,$4,$5,'active','ccbill',$6,$7,$7,$8)`, sub, merchantID, cust, prod, price, "fo-sub-"+sfx, start, elapsed)
		// The #691 shape: bounded per-period grant + ONE standing window.
		exec(`INSERT INTO openrails.grants (id,merchant_id,customer_id,kind,source_type,source_id,event,spec_snapshot,starts_at,ends_at)
		      VALUES ($1,$2,$3,'entitlement','subscription',$4,'grant',$5,$6,$7)`,
			grantID, merchantID, cust, sub.String(), []byte(`{"entitlements":["`+feat+`"]}`), start, elapsed)
		exec(`INSERT INTO openrails.entitlements (id,merchant_id,customer_id,entitlement,start_at,end_at,source_id,source_type,grant_id)
		      VALUES ($1,$2,$3,$4,$5,NULL,$6,'subscription',$7)`, entID, merchantID, cust, feat, start, sub, grantID)
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE id=$1`, entID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE customer_id=$1`, cust)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, sub)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, price)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, prod)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key = $2`, merchantID, "subscription:"+sub.String())
			return nil
		})
	})

	windowIntact := func(ctx context.Context) {
		t.Helper()
		var endAt, revokedAt, deletedAt *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT end_at, revoked_at, deleted_at FROM openrails.entitlements WHERE id=$1`, entID).Scan(&endAt, &revokedAt, &deletedAt))
		require.Nil(t, endAt, "standing window must stay open-ended")
		require.Nil(t, revokedAt, "standing window must not be revoked by convergence")
		require.Nil(t, deletedAt)
	}

	// Sweep #1: the lapsed evidence-less row parks `unknown`; access untouched.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &cust})
		require.NoError(t, err)
		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT status FROM openrails.subscriptions WHERE id=$1`, sub).Scan(&status))
		require.Equal(t, "unknown", status, "silence parks unknown")
		windowIntact(ctx)
		return nil
	}))

	// Repeated sweeps: no escalation, no access movement (#664 + #691).
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		for i := 0; i < 3; i++ {
			_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &cust})
			require.NoError(t, err)
		}
		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT status FROM openrails.subscriptions WHERE id=$1`, sub).Scan(&status))
		require.Equal(t, "unknown", status)
		windowIntact(ctx)
		return nil
	}))

	// Provider pull resolves renewed: paid-through advances, access continuous.
	newEnd := time.Now().UTC().Add(20 * 24 * time.Hour).Truncate(time.Second)
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		lc := subscriptions.NewSubscriptionLifecycleService(appDB, nil, nil, nil, nil, nil)
		row, err := appDB.Gen(ctx).GetSubscriptionByID(ctx, sub)
		require.NoError(t, err)
		m, err := models.SubscriptionFromGen(row)
		require.NoError(t, err)
		require.NoError(t, lc.ResolveUnknownSubscription(ctx, appDB, m, subscriptions.ResolveRenewed, &newEnd, time.Now().UTC()))
		var status string
		var periodEnd time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT status, current_period_ends_at FROM openrails.subscriptions WHERE id=$1`, sub).Scan(&status, &periodEnd))
		require.Equal(t, "active", status)
		require.WithinDuration(t, newEnd, periodEnd, time.Second, "paid-through fact advanced")
		windowIntact(ctx)
		return nil
	}))

	// And one more sweep over the healthy row: converged, access untouched.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &cust})
		require.NoError(t, err)
		windowIntact(ctx)
		return nil
	}))
}
