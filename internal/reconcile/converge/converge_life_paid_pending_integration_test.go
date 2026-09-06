//go:build integration

package converge

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestConverge_LifePaidPendingSubscriptionIsSurfacedNotCancelled(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	suffix := uuid.NewString()[:8]
	productID, priceID, subID, paymentID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	txnID := "pp-txn-" + suffix
	var customer uuid.UUID
	old := time.Now().UTC().Add(-5 * 24 * time.Hour)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		pspID := dbtest.EnsureTestPSP(ctx, t, appDB.Qx(ctx), merchantID, "nmi")
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id) VALUES ($1,$2,$2,$3,'{"premium": null}'::jsonb,$4)`,
			productID, "pp-prod-"+suffix, "pp-tier-"+suffix, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id) VALUES ($1,$2,9990000,'USD',720,true,$3)`, priceID, productID, merchantID)
		exec(`INSERT INTO openrails.subscriptions (id, price_id, product_id, status, rail, rail_subscription_id, started_at, created_at, entitlements_spec_snapshot, customer_id, merchant_id, psp_id)
		      VALUES ($1,$2,$3,'pending','nmi',$4,$5,$5,'{"premium": null}'::jsonb,$6,$7,$8)`,
			subID, priceID, productID, "pp-sub-"+suffix, old, customer, merchantID, pspID)
		exec(`INSERT INTO openrails.payments (id, customer_id, price_id, subscription_id, rail, psp_id, transaction_id, amount, list_amount, currency, status, money_movement, purchased_at, merchant_id)
		      VALUES ($1,$2,$3,$4,'nmi',$5,$6,9990000,9990000,'USD','completed','rail',$7,$8)`,
			paymentID, customer, priceID, subID, pspID, txnID, old, merchantID)
		exec(`INSERT INTO openrails.reconciliation_state (merchant_id, source_domain, fully_reconciled)
		      VALUES ($1,'subscriptions',true)
		      ON CONFLICT (merchant_id, source_domain) DO UPDATE SET fully_reconciled = true`, merchantID)
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key IN ($2, $3)`, merchantID, "subscription:"+subID.String(), "payment:"+paymentID.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_state WHERE merchant_id=$1 AND source_domain='subscriptions'`, merchantID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE id=$1`, paymentID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, subID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			return nil
		})
	})

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 0, res.AutoFixed, "a paid pending subscription must never be auto-cancelled")

		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT status::text FROM openrails.subscriptions WHERE id=$1`, subID).Scan(&status))
		require.Equal(t, "pending", status)

		rows, err := appDB.Qx(ctx).Query(ctx,
			`SELECT finding_type, status FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2 ORDER BY finding_type`,
			merchantID, "subscription:"+subID.String())
		require.NoError(t, err)
		defer rows.Close()
		var findings [][2]string
		for rows.Next() {
			var ftype, fstatus string
			require.NoError(t, rows.Scan(&ftype, &fstatus))
			findings = append(findings, [2]string{ftype, fstatus})
		}
		require.Equal(t, [][2]string{{"life.subscription.paid_pending", "requires_review"}}, findings)
		return nil
	}))
}
