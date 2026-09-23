//go:build integration

package converge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TestConverge_DeadSubRunwayGuard (#690 hazard fix): a user-cancelled sub's
// PAID RUNWAY window (bounded to period end) is NOT excess — the sweep leaves
// it alone until period end; a window extending PAST the entitled bound is
// bounded back to it (the missed #691 closure), never revoked-as-of-now.
func TestConverge_DeadSubRunwayGuard(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	suffix := uuid.NewString()[:8]
	productID, priceID, subID, entID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	var customer uuid.UUID
	now := time.Now().UTC()
	periodEnd := now.Add(10 * 24 * time.Hour) // paid through: 10 days of runway left

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO billing.products (id, key, display_name, tier_group, entitlements_spec, merchant_id)
		      VALUES ($1,$2,$2,$3,'{}'::jsonb,$4)`, productID, "rw-prod-"+suffix, "rw-tier-"+suffix, merchantID)
		exec(`INSERT INTO billing.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
		      VALUES ($1,$2,9990000,'USD',720,true,$3)`, priceID, productID, merchantID)
		pspID := dbtest.EnsureTestPSP(ctx, t, appDB.Qx(ctx), merchantID, "nmi")
		// User cancelled mid-period: cancelled now, ended_at = period end (the runway).
		exec(`INSERT INTO billing.subscriptions
		        (id, price_id, product_id, status, rail, rail_subscription_id,
		         current_period_starts_at, current_period_ends_at, started_at,
		         entitlements_spec_snapshot, customer_id, merchant_id, cancelled_at, cancel_type, ended_at, psp_id)
		      VALUES ($1,$2,$3,'cancelled','nmi',$4,$5,$6,$5,'{}'::jsonb,$7,$8,$9,'user',$10,$11)`,
			subID, priceID, productID, "rw-sub-"+suffix,
			now.Add(-20*24*time.Hour), periodEnd, customer, merchantID, now.Add(-time.Hour), periodEnd, pspID)
		// The proper #691 closure: window bounded exactly at period end.
		exec(`INSERT INTO billing.entitlements (id, customer_id, entitlement, start_at, end_at, source_id, source_type, merchant_id)
		      VALUES ($1,$2,$3,$4,$5,$6,'subscription',$7)`,
			entID, customer, "rw-feat-"+suffix, now.Add(-20*24*time.Hour), periodEnd, subID, merchantID)
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM billing.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "subscription:"+subID.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM billing.entitlements WHERE id=$1`, entID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM billing.subscriptions WHERE id=$1`, subID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM billing.prices WHERE id=$1`, priceID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM billing.products WHERE id=$1`, productID)
			return nil
		})
	})

	// Sweep 1: the runway is paid access — NO finding, window untouched.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Zero(t, res.Findings, "a user-cancelled sub's paid runway must not be flagged before period end")
		var endAt *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT end_at FROM billing.entitlements WHERE id=$1`, entID).Scan(&endAt))
		require.NotNil(t, endAt)
		require.WithinDuration(t, periodEnd, *endAt, time.Second, "runway window untouched")
		return nil
	}))

	// Extend the window past the entitled bound (simulated drift): the sweep
	// bounds it BACK to period end — the runway survives, the overrun is gone.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := appDB.Qx(ctx).Exec(ctx, `UPDATE billing.entitlements SET end_at=$1 WHERE id=$2`, now.Add(40*24*time.Hour), entID)
		require.NoError(t, err)
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings)
		require.Equal(t, 1, res.AutoFixed)
		var endAt, revokedAt *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT end_at, revoked_at FROM billing.entitlements WHERE id=$1`, entID).Scan(&endAt, &revokedAt))
		require.NotNil(t, endAt)
		require.WithinDuration(t, periodEnd, *endAt, 2*time.Second, "overrun bounded back to the entitled bound, not revoked-as-of-now")
		require.Nil(t, revokedAt, "the runway is preserved: closure, not revocation")
		return nil
	}))

	// Idempotent.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Zero(t, res.Findings)
		return nil
	}))
}

// TestConverge_DeriveEntitlementUnjustified: the two policy-ambiguous freeloader
// legs fire ADMIN findings with the revoke/admin-grant recommendation. Terminal
// subscription windows are unambiguous missed propagation and belong to the
// AUTO mismatch detector; stale-but-live sources (unknown/past_due standing
// windows, #691) and paid shapes never fire.
func TestConverge_DeriveEntitlementUnjustified(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	suffix := uuid.NewString()[:8]
	productID, priceID := uuid.New(), uuid.New()
	now := time.Now().UTC()
	var customer uuid.UUID

	// windows
	entMissingSub := uuid.New() // leg A: standing window, sub row missing
	entTerminal := uuid.New()   // AUTO: standing window, cancelled sub, bound passed
	entRunway := uuid.New()     // AUTO: standing window bounded to its paid runway
	entUnknown := uuid.New()    // negative: standing window of an unknown sub
	entPastDue := uuid.New()    // negative: standing window of a past_due sub
	entRefunded := uuid.New()   // leg C: one_off window, refunded payment, no grant
	entPaidOneOff := uuid.New() // negative: one_off window, completed payment
	danglingSub := uuid.New()   // no such row
	subTerminal, subRunway, subUnknown, subPastDue := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	payRefunded, payCompleted := uuid.New(), uuid.New()
	bound := now.Add(-10 * 24 * time.Hour)    // subTerminal's entitled bound (past)
	runwayEnd := now.Add(10 * 24 * time.Hour) // subRunway's paid-through (future)

	subjects := []string{
		"entitlement:" + entMissingSub.String(), "entitlement:" + entTerminal.String(),
		"entitlement:" + entRunway.String(), "entitlement:" + entUnknown.String(),
		"entitlement:" + entPastDue.String(), "entitlement:" + entRefunded.String(),
		"entitlement:" + entPaidOneOff.String(),
		"subscription:" + subTerminal.String(), "subscription:" + subRunway.String(),
	}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO billing.products (id, key, display_name, entitlements_spec, merchant_id)
		      VALUES ($1,$2,$2,'{}'::jsonb,$3)`, productID, "orph-prod-"+suffix, merchantID)
		exec(`INSERT INTO billing.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
		      VALUES ($1,$2,9990000,'USD',720,true,$3)`, priceID, productID, merchantID)
		pspID := dbtest.EnsureTestPSP(ctx, t, appDB.Qx(ctx), merchantID, "nmi")

		seedSub := func(id uuid.UUID, status string, periodEnd, endedAt *time.Time, nextRetry *time.Time) {
			exec(`INSERT INTO billing.subscriptions
			        (id, price_id, product_id, status, rail, rail_subscription_id,
			         current_period_starts_at, current_period_ends_at, started_at, next_retry_at,
			         entitlements_spec_snapshot, customer_id, merchant_id, cancelled_at, cancel_type, ended_at, psp_id)
			      VALUES ($1,$2,$3,$4,'nmi',$5,$6,$7,$6,$8,'{}'::jsonb,$9,$10,$11,$12,$13,$14)`,
				id, priceID, productID, status, "orph-"+id.String()[:8],
				now.Add(-40*24*time.Hour), periodEnd, nextRetry, customer, merchantID,
				timePtrOrNil(status == "cancelled", now.Add(-11*24*time.Hour)),
				strPtrOrNil(status == "cancelled", "user"), endedAt, pspID)
		}
		futureRetry := now.Add(24 * time.Hour)
		futurePeriod := now.Add(5 * 24 * time.Hour)
		seedSub(subTerminal, "cancelled", &bound, &bound, nil)
		seedSub(subRunway, "cancelled", &runwayEnd, &runwayEnd, nil)
		seedSub(subUnknown, "unknown", &bound, nil, nil)
		seedSub(subPastDue, "past_due", &futurePeriod, nil, &futureRetry)

		exec(`INSERT INTO billing.payments (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, psp_id)
		      VALUES ($1,$2,$3,$4,'nmi',$5,9990000,9990000,'USD','refunded',$6,$7)`,
			payRefunded, merchantID, customer, priceID, "orph-txn-r-"+suffix, now.Add(-5*24*time.Hour), pspID)
		exec(`INSERT INTO billing.payments (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, psp_id)
		      VALUES ($1,$2,$3,$4,'nmi',$5,9990000,9990000,'USD','completed',$6,$7)`,
			payCompleted, merchantID, customer, priceID, "orph-txn-c-"+suffix, now.Add(-4*24*time.Hour), pspID)

		seedEnt := func(id uuid.UUID, feature, sourceType string, sourceID uuid.UUID, endAt *time.Time) {
			exec(`INSERT INTO billing.entitlements (id, customer_id, entitlement, start_at, end_at, source_id, source_type, merchant_id)
			      VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				id, customer, feature, now.Add(-40*24*time.Hour), endAt, sourceID, sourceType, merchantID)
		}
		oneOffEnd := now.Add(30 * 24 * time.Hour)
		seedEnt(entMissingSub, "orph-a-"+suffix, "subscription", danglingSub, nil)
		seedEnt(entTerminal, "orph-b-"+suffix, "subscription", subTerminal, nil)
		seedEnt(entRunway, "orph-rw-"+suffix, "subscription", subRunway, nil)
		seedEnt(entUnknown, "orph-unk-"+suffix, "subscription", subUnknown, nil)
		seedEnt(entPastDue, "orph-pd-"+suffix, "subscription", subPastDue, nil)
		seedEnt(entRefunded, "orph-c-"+suffix, "one_off", payRefunded, &oneOffEnd)
		seedEnt(entPaidOneOff, "orph-paid-"+suffix, "one_off", payCompleted, &oneOffEnd)

		// subRunway's per-period grant still covers now (paid through runwayEnd):
		// its standing window must be bounded to that runway, not revoked now.
		gl := grants.New(appDB.Gen(ctx), merchantID)
		_, err := gl.Grant(ctx, grants.GrantInput{
			Customer: customer, Kind: grants.Entitlement, Source: grants.Subscription,
			SourceID: subRunway.String(), Spec: &grants.Spec{Entitlements: []string{"orph-rw-" + suffix}},
			StartsAt: now.Add(-20 * 24 * time.Hour), EndsAt: &runwayEnd,
		})
		require.NoError(t, err)
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM billing.reconciliation_findings WHERE merchant_id=$1 AND subject_key=ANY($2)`, merchantID, subjects)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM billing.entitlements WHERE customer_id=$1`, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM billing.grants WHERE customer_id=$1`, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM billing.payments WHERE id=ANY($1)`, []uuid.UUID{payRefunded, payCompleted})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM billing.subscriptions WHERE id=ANY($1)`, []uuid.UUID{subTerminal, subRunway, subUnknown, subPastDue})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM billing.prices WHERE id=$1`, priceID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM billing.products WHERE id=$1`, productID)
			return nil
		})
	})

	assertUnjustified := func(ctx context.Context, entID uuid.UUID, wantCause string) {
		t.Helper()
		var status, severity string
		var prose *string
		var evidence []byte
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status, severity, recommended_action, evidence FROM billing.reconciliation_findings
			 WHERE merchant_id=$1 AND finding_type='derive.entitlement.unjustified' AND subject_key=$2`,
			merchantID, "entitlement:"+entID.String()).Scan(&status, &severity, &prose, &evidence))
		require.Equal(t, "requires_review", status, "freeloaders are ADMIN surface-only")
		require.Equal(t, "high", severity)
		require.NotNil(t, prose, "prose recommendation persisted")
		var ev map[string]any
		require.NoError(t, json.Unmarshal(evidence, &ev))
		local, _ := ev["local"].(map[string]any)
		require.NotNil(t, local)
		require.Equal(t, wantCause, local["cause"])
		rec, ok := recommend.FromEvidence(ev)
		require.True(t, ok, "structured recommendation present")
		require.Equal(t, recommend.ActionRevokeEntitlement, rec.Action)
		require.Equal(t, entID.String(), rec.Params["entitlement_id"])
		require.Len(t, rec.Alternatives, 1)
		require.Equal(t, recommend.ActionRecordAdminGrant, rec.Alternatives[0].Action)
	}
	noUnjustified := func(ctx context.Context, entID uuid.UUID, label string) {
		t.Helper()
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM billing.reconciliation_findings
			 WHERE merchant_id=$1 AND finding_type='derive.entitlement.unjustified' AND subject_key=$2`,
			merchantID, "entitlement:"+entID.String()).Scan(&n))
		require.Zero(t, n, label)
	}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)

		assertUnjustified(ctx, entMissingSub, "missing_subscription")
		assertUnjustified(ctx, entRefunded, "refunded_payment")

		noUnjustified(ctx, entTerminal, "terminal standing access belongs to the AUTO mismatch detector")
		noUnjustified(ctx, entRunway, "terminal paid runway belongs to the AUTO mismatch detector")
		noUnjustified(ctx, entUnknown, "#691: stale unknown sub is not a freeloader")
		noUnjustified(ctx, entPastDue, "#691: past_due standing window is not a freeloader")
		noUnjustified(ctx, entPaidOneOff, "a completed payment justifies its window")

		// Partition: the LIVE dangling-sub window belongs to the unjustified check,
		// while standing terminal windows belong only to the AUTO mismatch check.
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM billing.reconciliation_findings
			 WHERE merchant_id=$1 AND finding_type='consistency.reference.source_reference' AND subject_key=$2`,
			merchantID, "entitlement:"+entMissingSub.String()).Scan(&n))
		require.Zero(t, n, "partition: live dangling window is unjustified-only")
		for _, subID := range []uuid.UUID{subTerminal, subRunway} {
			var status string
			require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
				`SELECT status FROM billing.reconciliation_findings
				 WHERE merchant_id=$1 AND finding_type='derive.grant_effect.mismatch' AND subject_key=$2`,
				merchantID, "subscription:"+subID.String()).Scan(&status))
			require.Equal(t, "auto_fixed", status, "terminal standing access must use the exact AUTO mismatch finding")
		}

		// Policy-ambiguous windows remain surface-only and untouched.
		for _, id := range []uuid.UUID{entMissingSub, entRefunded} {
			var revokedAt, endAt *time.Time
			require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT revoked_at, end_at FROM billing.entitlements WHERE id=$1`, id).Scan(&revokedAt, &endAt))
			require.Nil(t, revokedAt, "never auto-revoked (policy #690)")
		}
		for id, wantEnd := range map[uuid.UUID]time.Time{entTerminal: bound, entRunway: runwayEnd} {
			var revokedAt, endAt *time.Time
			require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT revoked_at, end_at FROM billing.entitlements WHERE id=$1`, id).Scan(&revokedAt, &endAt))
			require.Nil(t, revokedAt, "terminal access is bounded, not revoked")
			require.NotNil(t, endAt)
			require.WithinDuration(t, wantEnd, *endAt, 2*time.Second)
		}
		return nil
	}))

	// Idempotent: same findings, upserted in place.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM billing.reconciliation_findings
			 WHERE merchant_id=$1 AND finding_type='derive.entitlement.unjustified' AND subject_key=ANY($2)`,
			merchantID, subjects).Scan(&n))
		require.Equal(t, 2, n, "two freeloader shapes, one finding each, no duplicates")
		return nil
	}))
}
