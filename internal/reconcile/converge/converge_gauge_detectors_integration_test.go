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

// #690 detector tests: the freeloader detector (derive.entitlement.unjustified),
// the cross-month duplicate-ownership detector (consistency.duplicate.
// ownership), and the #691 runway guard on the dead-subs check.

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
		exec(`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id)
		      VALUES ($1,$2,$2,$3,'{}'::jsonb,$4)`, productID, "rw-prod-"+suffix, "rw-tier-"+suffix, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
		      VALUES ($1,$2,9990000,'usd',720,true,$3)`, priceID, productID, merchantID)
		// User cancelled mid-period: cancelled now, ended_at = period end (the runway).
		exec(`INSERT INTO openrails.subscriptions
		        (id, price_id, product_id, status, rail, rail_subscription_id,
		         current_period_starts_at, current_period_ends_at, started_at,
		         entitlements_spec_snapshot, customer_id, merchant_id, cancelled_at, cancel_type, ended_at)
		      VALUES ($1,$2,$3,'cancelled','nmi',$4,$5,$6,$5,'{}'::jsonb,$7,$8,$9,'user',$10)`,
			subID, priceID, productID, "rw-sub-"+suffix,
			now.Add(-20*24*time.Hour), periodEnd, customer, merchantID, now.Add(-time.Hour), periodEnd)
		// The proper #691 closure: window bounded exactly at period end.
		exec(`INSERT INTO openrails.entitlements (id, customer_id, entitlement, start_at, end_at, source_id, source_type, merchant_id)
		      VALUES ($1,$2,$3,$4,$5,$6,'subscription',$7)`,
			entID, customer, "rw-feat-"+suffix, now.Add(-20*24*time.Hour), periodEnd, subID, merchantID)
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "subscription:"+subID.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE id=$1`, entID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, subID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			return nil
		})
	})

	// Sweep 1: the runway is paid access — NO finding, window untouched.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Zero(t, res.Findings, "a user-cancelled sub's paid runway must not be flagged before period end")
		var endAt *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT end_at FROM openrails.entitlements WHERE id=$1`, entID).Scan(&endAt))
		require.NotNil(t, endAt)
		require.WithinDuration(t, periodEnd, *endAt, time.Second, "runway window untouched")
		return nil
	}))

	// Extend the window past the entitled bound (simulated drift): the sweep
	// bounds it BACK to period end — the runway survives, the overrun is gone.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := appDB.Qx(ctx).Exec(ctx, `UPDATE openrails.entitlements SET end_at=$1 WHERE id=$2`, now.Add(40*24*time.Hour), entID)
		require.NoError(t, err)
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings)
		require.Equal(t, 1, res.AutoFixed)
		var endAt, revokedAt *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT end_at, revoked_at FROM openrails.entitlements WHERE id=$1`, entID).Scan(&endAt, &revokedAt))
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

// TestConverge_DeriveEntitlementUnjustified: the three freeloader legs fire ADMIN
// findings with the revoke/admin-grant recommendation; stale-but-live sources
// (unknown/past_due standing windows, #691) and paid shapes never fire.
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
	entTerminal := uuid.New()   // leg B: standing window, cancelled sub, bound passed
	entRunway := uuid.New()     // leg B negative: standing window, cancelled sub, grant covers now
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
		exec(`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id)
		      VALUES ($1,$2,$2,$3,'{}'::jsonb,$4)`, productID, "orph-prod-"+suffix, "orph-tier-"+suffix, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
		      VALUES ($1,$2,9990000,'usd',720,true,$3)`, priceID, productID, merchantID)

		seedSub := func(id uuid.UUID, status string, periodEnd, endedAt *time.Time, nextRetry *time.Time) {
			exec(`INSERT INTO openrails.subscriptions
			        (id, price_id, product_id, status, rail, rail_subscription_id,
			         current_period_starts_at, current_period_ends_at, started_at, next_retry_at,
			         entitlements_spec_snapshot, customer_id, merchant_id, cancelled_at, cancel_type, ended_at)
			      VALUES ($1,$2,$3,$4,'nmi',$5,$6,$7,$6,$8,'{}'::jsonb,$9,$10,$11,$12,$13)`,
				id, priceID, productID, status, "orph-"+id.String()[:8],
				now.Add(-40*24*time.Hour), periodEnd, nextRetry, customer, merchantID,
				timePtrOrNil(status == "cancelled", now.Add(-11*24*time.Hour)),
				strPtrOrNil(status == "cancelled", "user"), endedAt)
		}
		futureRetry := now.Add(24 * time.Hour)
		futurePeriod := now.Add(5 * 24 * time.Hour)
		seedSub(subTerminal, "cancelled", &bound, &bound, nil)
		seedSub(subRunway, "cancelled", &runwayEnd, &runwayEnd, nil)
		seedSub(subUnknown, "unknown", &bound, nil, nil)
		seedSub(subPastDue, "past_due", &futurePeriod, nil, &futureRetry)

		exec(`INSERT INTO openrails.payments (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at)
		      VALUES ($1,$2,$3,$4,'nmi',$5,9990000,9990000,'usd','refunded',$6)`,
			payRefunded, merchantID, customer, priceID, "orph-txn-r-"+suffix, now.Add(-5*24*time.Hour))
		exec(`INSERT INTO openrails.payments (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at)
		      VALUES ($1,$2,$3,$4,'nmi',$5,9990000,9990000,'usd','completed',$6)`,
			payCompleted, merchantID, customer, priceID, "orph-txn-c-"+suffix, now.Add(-4*24*time.Hour))

		seedEnt := func(id uuid.UUID, feature, sourceType string, sourceID uuid.UUID, endAt *time.Time) {
			exec(`INSERT INTO openrails.entitlements (id, customer_id, entitlement, start_at, end_at, source_id, source_type, merchant_id)
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
		// its standing window is a missed closure but NOT yet freeloading.
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
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=ANY($2)`, merchantID, subjects)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE customer_id=$1`, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE customer_id=$1`, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE id=ANY($1)`, []uuid.UUID{payRefunded, payCompleted})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=ANY($1)`, []uuid.UUID{subTerminal, subRunway, subUnknown, subPastDue})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			return nil
		})
	})

	assertUnjustified := func(ctx context.Context, entID uuid.UUID, wantCause string) map[string]any {
		t.Helper()
		var status, severity string
		var prose *string
		var evidence []byte
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status, severity, recommended_action, evidence FROM openrails.reconciliation_findings
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
		return rec.Params
	}
	noUnjustified := func(ctx context.Context, entID uuid.UUID, label string) {
		t.Helper()
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.reconciliation_findings
			 WHERE merchant_id=$1 AND finding_type='derive.entitlement.unjustified' AND subject_key=$2`,
			merchantID, "entitlement:"+entID.String()).Scan(&n))
		require.Zero(t, n, label)
	}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)

		assertUnjustified(ctx, entMissingSub, "missing_subscription")
		params := assertUnjustified(ctx, entTerminal, "terminal_subscription_standing")
		asOf, hasAsOf := params["as_of"].(string)
		require.True(t, hasAsOf, "terminal-standing recommendation carries the entitled bound as as_of")
		parsed, err := time.Parse(time.RFC3339, asOf)
		require.NoError(t, err)
		require.WithinDuration(t, bound, parsed, 2*time.Second)
		assertUnjustified(ctx, entRefunded, "refunded_payment")

		noUnjustified(ctx, entRunway, "#691: paid runway (grant covers now) is not freeloading — no finding until past period end")
		noUnjustified(ctx, entUnknown, "#691: stale unknown sub is not a freeloader")
		noUnjustified(ctx, entPastDue, "#691: past_due standing window is not a freeloader")
		noUnjustified(ctx, entPaidOneOff, "a completed payment justifies its window")

		// Partition: the LIVE dangling-sub window belongs to the unjustified check —
		// the CON reference check must not double-fire on it; and the dead-subs
		// AUTO check must not touch STANDING windows of terminal subs.
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.reconciliation_findings
			 WHERE merchant_id=$1 AND finding_type='consistency.reference.source_reference' AND subject_key=$2`,
			merchantID, "entitlement:"+entMissingSub.String()).Scan(&n))
		require.Zero(t, n, "partition: live dangling window is unjustified-only")
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.reconciliation_findings
			 WHERE merchant_id=$1 AND finding_type='derive.grant_effect.mismatch' AND subject_key=$2`,
			merchantID, "subscription:"+subTerminal.String()).Scan(&n))
		require.Zero(t, n, "partition: standing window of a terminal sub is unjustified-only, not the dead-subs AUTO check")

		// Surface-only: every window is untouched.
		for _, id := range []uuid.UUID{entMissingSub, entTerminal, entRefunded} {
			var revokedAt, endAt *time.Time
			require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT revoked_at, end_at FROM openrails.entitlements WHERE id=$1`, id).Scan(&revokedAt, &endAt))
			require.Nil(t, revokedAt, "never auto-revoked (policy #690)")
		}
		return nil
	}))

	// Idempotent: same findings, upserted in place.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.reconciliation_findings
			 WHERE merchant_id=$1 AND finding_type='derive.entitlement.unjustified' AND subject_key=ANY($2)`,
			merchantID, subjects).Scan(&n))
		require.Equal(t, 3, n, "three freeloader shapes, one finding each, no duplicates")
		return nil
	}))
}

// TestConverge_ConDuplicateOwnership: two live paid ownership grants for the
// same (customer, product) — the cross-month double purchase — fire ONE
// critical ADMIN finding with the cancel_and_refund recommendation naming the
// later purchase; different products and sequential (terminated-then-repurchased)
// shapes never fire.
func TestConverge_ConDuplicateOwnership(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	suffix := uuid.NewString()[:8]
	now := time.Now().UTC()
	var customer uuid.UUID

	prodDup, prodSingle, prodSeq, prodSubShaped := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	priceID := uuid.New()
	pay1, pay2, pay3, pay4, pay5, pay6 := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	fakeSubID := uuid.New()

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		for i, pid := range []uuid.UUID{prodDup, prodSingle, prodSeq, prodSubShaped} {
			exec(`INSERT INTO openrails.products (id, key, display_name, entitlements_spec, merchant_id)
			      VALUES ($1,$2,$2,'{}'::jsonb,$3)`, pid, "dupown-"+suffix+"-"+string(rune('a'+i)), merchantID)
		}
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id)
		      VALUES ($1,$2,9990000,'usd',$3)`, priceID, prodDup, merchantID)
		seedPay := func(id uuid.UUID, txn string, at time.Time) {
			exec(`INSERT INTO openrails.payments (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at)
			      VALUES ($1,$2,$3,$4,'nmi',$5,9990000,9990000,'usd','completed',$6)`,
				id, merchantID, customer, priceID, txn, at)
		}
		// Every payment lands in a DIFFERENT month (all share one price →
		// product for provider_charge's grouping), so the month-scoped
		// duplicate.provider_charge check never fires — this test isolates the
		// cross-month gap that consistency.duplicate.ownership closes.
		seedPay(pay1, "do-1-"+suffix, now.Add(-65*24*time.Hour))
		seedPay(pay2, "do-2-"+suffix, now.Add(-24*time.Hour))
		seedPay(pay3, "do-3-"+suffix, now.Add(-95*24*time.Hour))
		seedPay(pay4, "do-4-"+suffix, now.Add(-125*24*time.Hour))
		seedPay(pay5, "do-5-"+suffix, now.Add(-155*24*time.Hour))
		seedPay(pay6, "do-6-"+suffix, now.Add(-35*24*time.Hour))

		gl := grants.New(appDB.Gen(ctx), merchantID)
		own := func(product uuid.UUID, source grants.SourceType, sourceID string, pay uuid.UUID, at time.Time) uuid.UUID {
			g, err := gl.Grant(ctx, grants.GrantInput{
				Customer: customer, Kind: grants.Ownership, Product: &product,
				Source: source, SourceID: sourceID, Payment: &pay, StartsAt: at,
			})
			require.NoError(t, err)
			return g.ID
		}
		// prodDup: two live purchase grants -> duplicate.
		own(prodDup, grants.Purchase, pay1.String(), pay1, now.Add(-60*24*time.Hour))
		own(prodDup, grants.Purchase, pay2.String(), pay2, now.Add(-24*time.Hour))
		// prodSingle: one grant -> clean.
		own(prodSingle, grants.Purchase, pay3.String(), pay3, now.Add(-90*24*time.Hour))
		// prodSeq: first terminated before the second was bought -> clean.
		first := own(prodSeq, grants.Purchase, pay4.String(), pay4, now.Add(-50*24*time.Hour))
		_, err := gl.Revoke(ctx, first, "refund/regrant test shape")
		require.NoError(t, err)
		own(prodSeq, grants.Purchase, pay5.String(), pay5, now.Add(-100*24*time.Hour))
		// prodSubShaped: purchase + LATER subscription-sourced grant -> duplicate
		// whose recommendation carries subscription_id.
		own(prodSubShaped, grants.Purchase, pay3.String(), pay3, now.Add(-90*24*time.Hour))
		own(prodSubShaped, grants.Subscription, fakeSubID.String(), pay6, now.Add(-2*24*time.Hour))
		return nil
	}))

	subjects := []string{
		"ownership:" + customer.String() + ":" + prodDup.String(),
		"ownership:" + customer.String() + ":" + prodSingle.String(),
		"ownership:" + customer.String() + ":" + prodSeq.String(),
		"ownership:" + customer.String() + ":" + prodSubShaped.String(),
	}
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=ANY($2)`, merchantID, subjects)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE customer_id=$1`, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE id=ANY($1)`, []uuid.UUID{pay1, pay2, pay3, pay4, pay5, pay6})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=ANY($1)`, []uuid.UUID{prodDup, prodSingle, prodSeq, prodSubShaped})
			return nil
		})
	})

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)

		load := func(product uuid.UUID) (string, string, *string, map[string]any, bool) {
			var status, severity string
			var prose *string
			var evidence []byte
			err := appDB.Qx(ctx).QueryRow(ctx,
				`SELECT status, severity, recommended_action, evidence FROM openrails.reconciliation_findings
				 WHERE merchant_id=$1 AND finding_type='consistency.duplicate.ownership' AND subject_key=$2`,
				merchantID, "ownership:"+customer.String()+":"+product.String()).Scan(&status, &severity, &prose, &evidence)
			if err != nil {
				return "", "", nil, nil, false
			}
			var ev map[string]any
			require.NoError(t, json.Unmarshal(evidence, &ev))
			return status, severity, prose, ev, true
		}

		// prodDup: critical duplicate with the later payment as refund target.
		status, severity, prose, ev, found := load(prodDup)
		require.True(t, found, "cross-month double purchase must surface")
		require.Equal(t, "requires_review", status)
		require.Equal(t, "critical", severity, "#690: a duplicate charge outranks a freeloader")
		require.NotNil(t, prose)
		require.Contains(t, *prose, pay1.String(), "prose names the earlier purchase")
		require.Contains(t, *prose, pay2.String(), "prose names the later purchase")
		rec, ok := recommend.FromEvidence(ev)
		require.True(t, ok)
		require.Equal(t, recommend.ActionCancelAndRefund, rec.Action)
		require.Equal(t, pay2.String(), rec.Params["refund_payment_id"], "later purchase is the default refund target")
		_, hasSub := rec.Params["subscription_id"]
		require.False(t, hasSub, "pure one-off duplicate is refund-only")

		// prodSubShaped: later grant is subscription-shaped -> cancel+refund.
		_, _, _, ev, found = load(prodSubShaped)
		require.True(t, found)
		rec, ok = recommend.FromEvidence(ev)
		require.True(t, ok)
		require.Equal(t, fakeSubID.String(), rec.Params["subscription_id"])
		require.Equal(t, pay6.String(), rec.Params["refund_payment_id"])

		// Negatives.
		_, _, _, _, found = load(prodSingle)
		require.False(t, found, "single purchase is clean")
		_, _, _, _, found = load(prodSeq)
		require.False(t, found, "sequential repurchase after termination is clean")

		// Surface-only: no grant terminated, no payment touched.
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.grants WHERE customer_id=$1 AND event IN ('revoke','expire','supersede')`, customer).Scan(&n))
		require.Equal(t, 1, n, "only the test's own seeded termination exists")
		return nil
	}))

	// Idempotent: still duplicates -> same two findings, upserted in place.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.reconciliation_findings
			 WHERE merchant_id=$1 AND finding_type='consistency.duplicate.ownership' AND subject_key=ANY($2)`,
			merchantID, subjects).Scan(&n))
		require.Equal(t, 2, n)
		return nil
	}))
}
