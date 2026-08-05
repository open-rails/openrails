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

// #511 Phase D (LIFE plane): an expired, non-terminal checkout session is
// detected as `life.checkout_session.stale` and cleaned up (marked expired). It's
// EXCESS but time-driven, so AUTO and NOT confirmed-absence gated. Idempotent.
func TestConverge_LifeCheckoutSessionStale(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	suffix := uuid.NewString()[:8]
	productID, priceID, sessionID := uuid.New(), uuid.New(), uuid.New()
	var customer uuid.UUID

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id)
		      VALUES ($1, $2, $2, $3, '{}'::jsonb, $4)`,
			productID, "life-prod-"+suffix, "life-tier-"+suffix, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
		      VALUES ($1, $2, 9990000, 'USD', 720, true, $3)`, priceID, productID, merchantID)
		pspID := dbtest.EnsureTestPSP(ctx, t, appDB.Qx(ctx), merchantID, "nmi")
		// A checkout session that expired an hour ago but is still 'created'.
		exec(`INSERT INTO openrails.checkout_sessions
		        (id, price_id, mode, rail, status, amount, currency, expires_at, merchant_id, customer_id, psp_id)
		      VALUES ($1, $2, 'one_off', 'nmi', 'created', 9990000, 'USD', $3, $4, $5, $6)`,
			sessionID, priceID, time.Now().Add(-1*time.Hour), merchantID, customer, pspID)
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "checkout_session:"+sessionID.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.checkout_sessions WHERE id=$1`, sessionID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			return nil
		})
	})

	sessionStatus := func(ctx context.Context) string {
		var s string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT status FROM openrails.checkout_sessions WHERE id=$1`, sessionID).Scan(&s))
		return s
	}

	// Converge (customer-scoped to isolate from other tests' sessions) → clean up.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings)
		require.Equal(t, 1, res.AutoFixed)
		require.Equal(t, "expired", sessionStatus(ctx), "stale session marked expired")

		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type='life.checkout_session.stale' AND subject_key=$2`,
			merchantID, "checkout_session:"+sessionID.String()).Scan(&status))
		require.Equal(t, "auto_fixed", status)
		return nil
	}))

	// Idempotent: already expired → not stale → no finding.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 0, res.Findings, "cleaned up → converged, no finding")
		return nil
	}))
}
