//go:build integration

package riverjobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
)

// #511 Phase E (end-to-end): the ConvergeSweepWorker is the background invocation
// of the Convergence Engine. This test seeds two unrelated kinds of internal-plane
// drift under the test merchant — a stale checkout session and a grace-exhausted
// subscription — runs the worker exactly as River would (no inline hook touches
// either row), and asserts BOTH were converged (#664: the sub is parked as
// `unknown`, never cancelled). It proves the wired system works: the periodic
// worker enumerates active merchants and drives reconcile.Converge across them,
// remediating drift that no request path would have caught.
func TestConvergeSweepWorker_RemediatesDriftAcrossMerchant(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := dbtest.WithTestMerchant(context.Background())
	dbtest.EnsureTestMerchant(baseCtx, t, dbi.Pool())

	suffix := uuid.NewString()[:8]
	feature := "feat_" + suffix
	productID, priceID := uuid.New(), uuid.New()
	sessionID := uuid.New()
	subID, entID := uuid.New(), uuid.New()
	var customer uuid.UUID
	graceEnd := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second) // grace elapsed

	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, dbi.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := dbi.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id) VALUES ($1,$2,$2,$3,'{}'::jsonb,$4)`,
			productID, "sweep-prod-"+suffix, "sweep-tier-"+suffix, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id) VALUES ($1,$2,999,'usd',720,true,$3)`, priceID, productID, merchantID)
		// drift #1: a checkout session that expired but is still 'created'
		exec(`INSERT INTO openrails.checkout_sessions (id, price_id, mode, rail, status, amount, currency, expires_at, merchant_id, customer_id)
		      VALUES ($1,$2,'one_off','nmi','created',999,'usd',$3,$4,$5)`,
			sessionID, priceID, time.Now().Add(-1*time.Hour), merchantID, customer)
		// drift #2: a past_due subscription whose grace window already elapsed
		exec(`INSERT INTO openrails.subscriptions (id, price_id, product_id, status, rail, rail_subscription_id, current_period_starts_at, current_period_ends_at, started_at, grace_ends_at, entitlements_spec_snapshot, customer_id, merchant_id)
		      VALUES ($1,$2,$3,'past_due','nmi',$4,$5,$6,$5,$7,'{}'::jsonb,$8,$9)`,
			subID, priceID, productID, "sweep-sub-"+suffix,
			time.Now().Add(-33*24*time.Hour), time.Now().Add(-3*time.Hour), graceEnd, customer, merchantID)
		exec(`INSERT INTO openrails.entitlements (id, customer_id, entitlement, start_at, end_at, source_id, source_type, merchant_id)
		      VALUES ($1,$2,$3,$4,$5,$6,'subscription',$7)`,
			entID, customer, feature, time.Now().Add(-33*24*time.Hour), time.Now().Add(27*24*time.Hour), subID, merchantID)
		return nil
	}))

	t.Cleanup(func() {
		_ = dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=ANY($2)`,
				merchantID, []string{"checkout_session:" + sessionID.String(), "subscription:" + subID.String()})
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE id=$1`, entID)
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.checkout_sessions WHERE id=$1`, sessionID)
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, subID)
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			return nil
		})
	})

	// Run the worker exactly as River would — no merchant on the ctx, no inline hook.
	worker := ConvergeSweepWorker{DB: dbi}
	require.NoError(t, worker.Work(context.Background(), &river.Job[ConvergeSweepArgs]{}))

	// Both drift items converged.
	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var sessionStatus string
		require.NoError(t, dbi.Qx(ctx).QueryRow(ctx, `SELECT status FROM openrails.checkout_sessions WHERE id=$1`, sessionID).Scan(&sessionStatus))
		require.Equal(t, "expired", sessionStatus, "sweep converged the stale checkout session")

		// #664: parked as unknown, never cancelled, access intact.
		var subStatus string
		require.NoError(t, dbi.Qx(ctx).QueryRow(ctx, `SELECT status::text FROM openrails.subscriptions WHERE id=$1`, subID).Scan(&subStatus))
		require.Equal(t, "unknown", subStatus, "sweep parked the grace-exhausted subscription for verification")

		var revokedAt *time.Time
		require.NoError(t, dbi.Qx(ctx).QueryRow(ctx, `SELECT revoked_at FROM openrails.entitlements WHERE id=$1`, entID).Scan(&revokedAt))
		require.Nil(t, revokedAt, "entitlement NOT revoked on a guess")

		// findings were recorded as auto_fixed
		var n int
		require.NoError(t, dbi.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND status='auto_fixed' AND subject_key=ANY($2)`,
			merchantID, []string{"checkout_session:" + sessionID.String(), "subscription:" + subID.String()}).Scan(&n))
		require.Equal(t, 2, n, "both findings recorded auto_fixed")
		return nil
	}))

	// Idempotent: a second sweep finds nothing more to fix for these rows.
	require.NoError(t, worker.Work(context.Background(), &river.Job[ConvergeSweepArgs]{}))
}
