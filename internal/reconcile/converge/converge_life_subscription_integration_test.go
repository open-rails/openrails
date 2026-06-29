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

// #511 Phase D (LIFE plane): a past_due subscription whose grace window has
// elapsed is detected as `life.subscription.grace_exhausted` and converged to
// terminal — cancelled NOW, with its entitlement revoked AS-OF grace end
// (converge-not-replay: no missed dunning charges are re-run). Idempotent.
func TestConverge_LifeSubscriptionGraceExhausted(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	suffix := uuid.NewString()[:8]
	feature := "feat_" + suffix
	productID, priceID, subID, entID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	var customer uuid.UUID

	graceEnd := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second) // grace elapsed 2h ago

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id)
		      VALUES ($1, $2, $2, $3, '{}'::jsonb, $4)`, productID, "ge-prod-"+suffix, "ge-tier-"+suffix, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
		      VALUES ($1, $2, 9990000, 'usd', 720, true, $3)`, priceID, productID, merchantID)
		exec(`INSERT INTO openrails.subscriptions
		        (id, price_id, product_id, status, rail, rail_subscription_id,
		         current_period_starts_at, current_period_ends_at, started_at, grace_ends_at,
		         entitlements_spec_snapshot, customer_id, merchant_id)
		      VALUES ($1, $2, $3, 'past_due', 'nmi', $4, $5, $6, $5, $7, '{}'::jsonb, $8, $9)`,
			subID, priceID, productID, "ge-sub-"+suffix,
			time.Now().Add(-33*24*time.Hour), time.Now().Add(-3*time.Hour), graceEnd, customer, merchantID)
		exec(`INSERT INTO openrails.entitlements (id, customer_id, entitlement, start_at, end_at, source_id, source_type, merchant_id)
		      VALUES ($1, $2, $3, $4, $5, $6, 'subscription', $7)`,
			entID, customer, feature, time.Now().Add(-33*24*time.Hour), time.Now().Add(27*24*time.Hour), subID, merchantID)
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "subscription:"+subID.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, subID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			return nil
		})
	})

	// Converge → detect grace exhaustion → cancel + revoke as-of grace end.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings)
		require.Equal(t, 1, res.AutoFixed)

		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT status::text FROM openrails.subscriptions WHERE id=$1`, subID).Scan(&status))
		require.Equal(t, "cancelled", status, "grace-exhausted sub is cancelled")

		var revokedAt *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT revoked_at FROM openrails.entitlements WHERE id=$1`, entID).Scan(&revokedAt))
		require.NotNil(t, revokedAt, "entitlement revoked")
		require.WithinDuration(t, graceEnd, *revokedAt, time.Second, "revoked AS-OF grace end, not convergence time")

		var fstatus string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type='life.subscription.grace_exhausted' AND subject_key=$2`,
			merchantID, "subscription:"+subID.String()).Scan(&fstatus))
		require.Equal(t, "auto_fixed", fstatus)
		return nil
	}))

	// Idempotent: cancelled → no longer past_due → no finding.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 0, res.Findings, "terminal → converged, no finding")
		return nil
	}))
}
