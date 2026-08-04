//go:build integration

package converge

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #511/#842 Phase D (LIFE): a `pending` subscription that never confirmed
// within the threshold waits for authoritative subscription coverage. Time
// alone cannot prove the remote subscription is absent.
func TestConverge_LifeSubscriptionPendingStaleWaitsForSourceProof(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	suffix := uuid.NewString()[:8]
	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
	var customer uuid.UUID
	old := time.Now().UTC().Add(-5 * 24 * time.Hour) // 5 days ago (> 72h threshold)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id) VALUES ($1,$2,$2,$3,'{}'::jsonb,$4)`,
			productID, "ps-prod-"+suffix, "ps-tier-"+suffix, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id) VALUES ($1,$2,9990000,'USD',720,true,$3)`, priceID, productID, merchantID)
		exec(`INSERT INTO openrails.subscriptions (id, price_id, product_id, status, rail, rail_subscription_id, started_at, created_at, entitlements_spec_snapshot, customer_id, merchant_id)
		      VALUES ($1,$2,$3,'pending','nmi',$4,$5,$5,'{}'::jsonb,$6,$7)`,
			subID, priceID, productID, "ps-sub-"+suffix, old, customer, merchantID)
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "subscription:"+subID.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_state WHERE merchant_id=$1 AND source_domain='subscriptions'`, merchantID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, subID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			return nil
		})
	})

	// #842: pending_stale cancels because a confirmation did NOT arrive — an
	// absence — so it is now behind the §3.2 confirmed-absence gate. Until the
	// merchant's `subscriptions` domain is proven fully reconciled, a delayed or
	// dropped webhook must not be read as "the customer never paid".
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings)
		require.Equal(t, 0, res.AutoFixed, "ungated retraction: the gate must hold it")
		require.Equal(t, 1, res.ReconcileRequired)
		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT status::text FROM openrails.subscriptions WHERE id=$1`, subID).Scan(&status))
		require.Equal(t, "pending", status, "the subscription must survive an unproven absence")
		var findingStatus string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`,
			merchantID, "subscription:"+subID.String()).Scan(&findingStatus))
		require.Equal(t, "reconcile_required", findingStatus)
		return nil
	}))

	// Prove the domain (what a completed exhaustive pull does) and the same
	// repair proceeds.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := appDB.Qx(ctx).Exec(ctx,
			`INSERT INTO openrails.reconciliation_state (merchant_id, source_domain, fully_reconciled, last_full_pull_at)
			 VALUES ($1,'subscriptions',true,now())
			 ON CONFLICT (merchant_id, source_domain) DO UPDATE SET fully_reconciled = true`, merchantID)
		return err
	}))
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings)
		require.Equal(t, 1, res.AutoFixed, "proven provider coverage releases the held repair")
		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT status::text FROM openrails.subscriptions WHERE id=$1`, subID).Scan(&status))
		require.Equal(t, "cancelled", status)
		return nil
	}))
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 0, res.Findings, "cancelled subscription is converged")
		return nil
	}))
}

// #511 Phase D (LIFE): a provider intent that won't auto-retry (terminal) is
// surfaced as `life.provider_intent.abandoned` — ADMIN, no auto-repair.
func TestConverge_LifeProviderIntentAbandoned(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	suffix := uuid.NewString()[:8]
	productID, priceID, subID, intentID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	var customer uuid.UUID

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id) VALUES ($1,$2,$2,$3,'{}'::jsonb,$4)`,
			productID, "pi-prod-"+suffix, "pi-tier-"+suffix, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id) VALUES ($1,$2,9990000,'USD',720,true,$3)`, priceID, productID, merchantID)
		exec(`INSERT INTO openrails.subscriptions (id, price_id, product_id, status, rail, rail_subscription_id, started_at, entitlements_spec_snapshot, customer_id, merchant_id)
		      VALUES ($1,$2,$3,'active','nmi',$4,now(),'{}'::jsonb,$5,$6)`, subID, priceID, productID, "pi-sub-"+suffix, customer, merchantID)
		// a provider action that failed terminally and won't auto-retry
		exec(`INSERT INTO openrails.rail_intents (id, merchant_id, rail, intent_type, idempotency_key, status, origin, subscription_id)
		      VALUES ($1,$2,'nmi','cancel_subscription',$3,'failed_terminal','system',$4)`, intentID, merchantID, "pi-key-"+suffix, subID)
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "provider_intent:"+intentID.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.rail_intents WHERE id=$1`, intentID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, subID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			return nil
		})
	})

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer, Subscription: &subID})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings)
		require.Equal(t, 1, res.RequiresReview, "abandoned intent is surfaced, not auto-fixed")
		require.Equal(t, 0, res.AutoFixed)

		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type='life.provider_intent.abandoned' AND subject_key=$2`,
			merchantID, "provider_intent:"+intentID.String()).Scan(&status))
		require.Equal(t, "requires_review", status)

		// surface-only: the intent itself is untouched
		var istatus string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT status FROM openrails.rail_intents WHERE id=$1`, intentID).Scan(&istatus))
		require.Equal(t, "failed_terminal", istatus)
		return nil
	}))
}

// #511 Phase D (LIFE): an active sub past its period end (missed renewal) is
// converged into dunning (past_due) with a grace window dated to the period end.
// Recent overdue → grace still in the future → stays past_due (idempotent).
func TestConverge_LifeSubscriptionPeriodOverdue(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	suffix := uuid.NewString()[:8]
	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
	var customer uuid.UUID
	periodEnd := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second) // ended 1h ago

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id) VALUES ($1,$2,$2,$3,'{}'::jsonb,$4)`,
			productID, "po-prod-"+suffix, "po-tier-"+suffix, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id) VALUES ($1,$2,9990000,'USD',720,true,$3)`, priceID, productID, merchantID)
		// #664: period_overdue needs vault + ownership evidence — seed a payment
		// method and a completed payment that opened the current period.
		pmID := uuid.New()
		exec(`INSERT INTO openrails.payment_methods (id, merchant_id, customer_id, rail, rail_customer_ref, rail_method_ref, initial_transaction_id) VALUES ($1,$2,$3,'nmi','po-cust','po-vault','po-tx')`, pmID, merchantID, customer)
		exec(`INSERT INTO openrails.subscriptions (id, price_id, product_id, status, rail, rail_subscription_id, payment_method_id, current_period_starts_at, current_period_ends_at, started_at, entitlements_spec_snapshot, customer_id, merchant_id)
		      VALUES ($1,$2,$3,'active','nmi',$4,$5,$6,$7,$6,'{}'::jsonb,$8,$9)`,
			subID, priceID, productID, "po-sub-"+suffix, pmID, periodEnd.Add(-30*24*time.Hour), periodEnd, customer, merchantID)
		exec(`INSERT INTO openrails.payments (id, merchant_id, customer_id, price_id, subscription_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at)
		      VALUES ($1,$2,$3,$4,$5,'nmi',$6,9990000,9990000,'USD','completed',$7)`,
			uuid.New(), merchantID, customer, priceID, subID, "po-pay-"+suffix, periodEnd.Add(-30*24*time.Hour))
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "subscription:"+subID.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE subscription_id=$1`, subID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, subID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			return nil
		})
	})

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings)
		require.Equal(t, 1, res.AutoFixed)
		var status string
		var graceEnds *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT status::text, grace_ends_at FROM openrails.subscriptions WHERE id=$1`, subID).Scan(&status, &graceEnds))
		require.Equal(t, "past_due", status)
		require.NotNil(t, graceEnds)
		require.WithinDuration(t, periodEnd.Add(reconcile.PeriodGrace), *graceEnds, time.Second, "grace dated to period end + cap")
		return nil
	}))
	// Second pass: the sub is now past_due with a future grace window but no retry
	// scheduled, so life.subscription.dunning_overdue composes on top — it
	// establishes the retry schedule (a separate invariant from "enter dunning").
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings, "period_overdue → past_due composes into dunning_overdue")
		var retry *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT next_retry_at FROM openrails.subscriptions WHERE id=$1`, subID).Scan(&retry))
		require.NotNil(t, retry, "dunning_overdue scheduled the retry")
		return nil
	}))
	// Third pass: past_due + future grace + a live retry schedule → true fixpoint.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 0, res.Findings, "scheduled past_due → converged, no finding")
		return nil
	}))
}

// #511 Phase D (LIFE): a past_due subscription still within its grace window but
// with NO retry scheduled (a stalled dunning schedule) is detected as
// life.subscription.dunning_overdue and the retry schedule is re-established.
// Converge-not-replay: it schedules the NEXT retry, it does not re-run missed
// attempts. Idempotent once a retry is scheduled.
func TestConverge_LifeSubscriptionDunningOverdue(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	suffix := uuid.NewString()[:8]
	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
	var customer uuid.UUID
	periodEnd := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second) // ended 1h ago
	graceEnd := time.Now().UTC().Add(47 * time.Hour).Truncate(time.Second)  // grace still open

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id) VALUES ($1,$2,$2,$3,'{}'::jsonb,$4)`,
			productID, "do-prod-"+suffix, "do-tier-"+suffix, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id) VALUES ($1,$2,9990000,'USD',720,true,$3)`, priceID, productID, merchantID)
		// past_due, grace still open, but next_retry_at NULL: schedule stalled.
		exec(`INSERT INTO openrails.subscriptions (id, price_id, product_id, status, rail, rail_subscription_id, current_period_starts_at, current_period_ends_at, started_at, grace_ends_at, entitlements_spec_snapshot, customer_id, merchant_id)
		      VALUES ($1,$2,$3,'past_due','nmi',$4,$5,$6,$5,$7,'{}'::jsonb,$8,$9)`,
			subID, priceID, productID, "do-sub-"+suffix, periodEnd.Add(-30*24*time.Hour), periodEnd, graceEnd, customer, merchantID)
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "subscription:"+subID.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, subID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			return nil
		})
	})

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings)
		require.Equal(t, 1, res.AutoFixed)

		var retry *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT next_retry_at FROM openrails.subscriptions WHERE id=$1`, subID).Scan(&retry))
		require.NotNil(t, retry, "retry schedule re-established")
		require.WithinDuration(t, time.Now().UTC(), *retry, 5*time.Second, "scheduled the NEXT retry (now), not a replayed past attempt")

		// the sub stays past_due (we resumed dunning, we did not cancel)
		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT status::text FROM openrails.subscriptions WHERE id=$1`, subID).Scan(&status))
		require.Equal(t, "past_due", status)

		var fstatus string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type='life.subscription.dunning_overdue' AND subject_key=$2`,
			merchantID, "subscription:"+subID.String()).Scan(&fstatus))
		require.Equal(t, "auto_fixed", fstatus)
		return nil
	}))

	// Idempotent: retry now scheduled → no longer stalled → no finding.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 0, res.Findings, "retry scheduled → converged, no finding")
		return nil
	}))
}
