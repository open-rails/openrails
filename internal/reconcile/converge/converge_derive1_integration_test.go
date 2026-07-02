//go:build integration

package converge

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #631 derive-1: a stored, ungranted ACTIVE subscription for a grantable product
// is auto-materialized into a grant (source_type=subscription) + an entitlement
// window spanning its period. A second Converge is a no-op (source-keyed
// idempotency). A PENDING subscription is NOT derived (only active/cancelled grant
// access). This is exactly the migrate/convergence split: the migrate moves the
// subscription, the engine derives the access.
func TestConverge_DeriveGrantMissing_Subscription(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	sfx := uuid.NewString()[:8]

	prod := uuid.New()
	price := uuid.New()
	subActive, subPending := uuid.New(), uuid.New()
	var custActive, custPending uuid.UUID
	start := time.Now().UTC().Add(-240 * time.Hour) // -10d
	end := time.Now().UTC().Add(480 * time.Hour)    // +20d

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		custActive = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		custPending = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, entitlements_spec, merchant_id) VALUES ($1,$2,$2,'{"premium":null}'::jsonb,$3)`, prod, "d1-"+sfx, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id) VALUES ($1,$2,5000000,'usd',$3)`, price, prod, merchantID)
		exec(`INSERT INTO openrails.subscriptions (id, merchant_id, customer_id, product_id, price_id, status, rail, started_at, current_period_starts_at, current_period_ends_at)
		      VALUES ($1,$2,$3,$4,$5,'active','mobius',$6,$6,$7)`, subActive, merchantID, custActive, prod, price, start, end)
		exec(`INSERT INTO openrails.subscriptions (id, merchant_id, customer_id, product_id, price_id, status, rail, started_at, current_period_starts_at, current_period_ends_at)
		      VALUES ($1,$2,$3,$4,$5,'pending','mobius',$6,$6,$7)`, subPending, merchantID, custPending, prod, price, start, end)
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			for _, c := range []uuid.UUID{custActive, custPending} {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key LIKE 'subscription:%'`, merchantID)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2`, merchantID, c)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2`, merchantID, c)
			}
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=ANY($1)`, []uuid.UUID{subActive, subPending})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, price)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, prod)
			return nil
		})
	})

	liveGrants := func(ctx context.Context, c uuid.UUID) int {
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2 AND event='grant' AND source_type='subscription'`,
			merchantID, c).Scan(&n))
		return n
	}
	liveEnts := func(ctx context.Context, c uuid.UUID) int {
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2 AND revoked_at IS NULL AND deleted_at IS NULL`,
			merchantID, c).Scan(&n))
		return n
	}

	// First converge — derives the active subscription.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &custActive})
		require.NoError(t, err)
		require.Equal(t, 1, liveGrants(ctx, custActive), "active sub → one subscription grant")
		require.Equal(t, 1, liveEnts(ctx, custActive), "active sub → one entitlement window")

		// Verify the entitlement window matches the subscription period + source.
		var entStart, entEnd time.Time
		var srcType string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT start_at, end_at, source_type FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2 AND entitlement='premium' AND revoked_at IS NULL`,
			merchantID, custActive).Scan(&entStart, &entEnd, &srcType))
		require.WithinDuration(t, start, entStart, time.Second)
		require.WithinDuration(t, end, entEnd, time.Second)
		require.Equal(t, "subscription", srcType)
		return nil
	}))

	// Second converge — idempotent no-op (no new grant/entitlement).
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &custActive})
		require.NoError(t, err)
		require.Equal(t, 1, liveGrants(ctx, custActive), "re-run is a no-op")
		require.Equal(t, 1, liveEnts(ctx, custActive), "re-run is a no-op")
		return nil
	}))

	// Pending subscription is never derived.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &custPending})
		require.NoError(t, err)
		require.Equal(t, 0, liveGrants(ctx, custPending), "pending sub → no grant")
		require.Equal(t, 0, liveEnts(ctx, custPending), "pending sub → no entitlement")
		return nil
	}))
}

// #631/#695 derive-1 overlap policy: when the customer already holds a LIVE
// entitlement for the feature whose window overlaps the subscription period,
// derive-1 STILL records the grant (provenance — detection keys on it) but
// derive-2 projects NO window (absent-by-overlap; the exclusion constraint is
// never tripped). The second sweep is findings-free: the recorded grant makes
// derive.subscription.missing converge and grant_effect.missing mirrors the
// overlap no-op.
func TestConverge_DeriveSubscription_OverlapRecordsGrantWithoutWindow(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	sfx := uuid.NewString()[:8]

	prod := uuid.New()
	price := uuid.New()
	sub := uuid.New()
	var customer uuid.UUID
	start := time.Now().UTC().Add(-240 * time.Hour)
	end := time.Now().UTC().Add(480 * time.Hour)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, entitlements_spec, merchant_id) VALUES ($1,$2,$2,'{"premium":null}'::jsonb,$3)`, prod, "d1o-"+sfx, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id) VALUES ($1,$2,5000000,'usd',$3)`, price, prod, merchantID)
		exec(`INSERT INTO openrails.subscriptions (id, merchant_id, customer_id, product_id, price_id, status, rail, started_at, current_period_starts_at, current_period_ends_at)
		      VALUES ($1,$2,$3,$4,$5,'active','mobius',$6,$6,$7)`, sub, merchantID, customer, prod, price, start, end)
		// Pre-existing live entitlement that OVERLAPS the subscription window.
		exec(`INSERT INTO openrails.entitlements (id, merchant_id, customer_id, entitlement, start_at, end_at, source_type, source_id)
		      VALUES ($1,$2,$3,'premium',$4,$5,'admin',$6)`, uuid.New(), merchantID, customer, start.Add(-24*time.Hour), end.Add(24*time.Hour), uuid.New())
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "subscription:"+sub.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, sub)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, price)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, prod)
			return nil
		})
	})

	// Sweep 1: the grant is recorded (provenance, frozen [start,end) window on the
	// grant row) but NO entitlement window materializes for it.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Equal(t, 1, res.AutoFixed, "derive.subscription.missing auto-fixed once")
		var grantN int
		var gStart, gEnd time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2 AND source_type='subscription' AND event='grant'`,
			merchantID, customer).Scan(&grantN))
		require.Equal(t, 1, grantN, "overlapping window → grant STILL recorded (#695 provenance)")
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT starts_at, ends_at FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2 AND source_type='subscription' AND event='grant'`,
			merchantID, customer).Scan(&gStart, &gEnd))
		require.WithinDuration(t, start, gStart, time.Second, "grant carries the frozen subscription window")
		require.WithinDuration(t, end, gEnd, time.Second)
		var entN int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2 AND grant_id IS NOT NULL`,
			merchantID, customer).Scan(&entN))
		require.Equal(t, 0, entN, "no window materialized for the overlapped grant (constraint never tripped)")
		return nil
	}))

	// Sweep 2: converged — the recorded grant quiets derive.subscription.missing
	// and grant_effect.missing treats the window as deliberately absent-by-overlap.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Zero(t, res.Findings, "second sweep is findings-free (no flap)")
		return nil
	}))
}

// #695 flap regression, subscription shape (the doujins import): a customer with
// an ACTIVE auto-renew sub projecting a STANDING window (#691, end_at NULL) plus
// an old CANCELLED sub for the same product/feature whose historical period sits
// inside the standing window and has NO grant. Pre-#695 this flapped forever:
// the overlap-skip never recorded a grant, so derive.subscription.missing
// re-fired + re-"auto-fixed" every sweep. Now: sweep 1 records the grant (no new
// window), sweep 2 emits ZERO findings; access is unchanged throughout (standing
// window intact, no bounded window added) and grant_effect.missing stays quiet
// on the deliberately window-less grant.
func TestConverge_DeriveFlap_StandingWindowPlusCancelledSub(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	sfx := uuid.NewString()[:8]
	feat := "flap-feat-" + sfx

	prod, price := uuid.New(), uuid.New()
	activeSub, cancelledSub := uuid.New(), uuid.New()
	activeGrant, standingEnt := uuid.New(), uuid.New()
	var cust uuid.UUID
	now := time.Now().UTC()
	subStart := now.Add(-130 * 24 * time.Hour)
	periodStart := now.Add(-10 * 24 * time.Hour)
	periodEnd := now.Add(20 * 24 * time.Hour)
	oldStart := now.Add(-100 * 24 * time.Hour) // fully inside the standing window
	oldEnd := now.Add(-70 * 24 * time.Hour)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		cust = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id) VALUES ($1,$2,$2,$3,$4)`,
			prod, "flap-prod-"+sfx, []byte(`{"`+feat+`": null}`), merchantID)
		exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,access_duration_hours,auto_renew,merchant_id) VALUES ($1,$2,5000000,'usd',720,true,$3)`,
			price, prod, merchantID)
		// ACTIVE auto-renew sub in a RUNNING period, with the #691 shape already
		// projected: bounded per-period grant + ONE standing window.
		exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,started_at,current_period_starts_at,current_period_ends_at)
		      VALUES ($1,$2,$3,$4,$5,'active','ccbill',$6,$7,$8,$9)`,
			activeSub, merchantID, cust, prod, price, "flap-live-"+sfx, subStart, periodStart, periodEnd)
		exec(`INSERT INTO openrails.grants (id,merchant_id,customer_id,kind,source_type,source_id,event,spec_snapshot,starts_at,ends_at)
		      VALUES ($1,$2,$3,'entitlement','subscription',$4,'grant',$5,$6,$7)`,
			activeGrant, merchantID, cust, activeSub.String(), []byte(`{"entitlements":["`+feat+`"]}`), periodStart, periodEnd)
		exec(`INSERT INTO openrails.entitlements (id,merchant_id,customer_id,entitlement,start_at,end_at,source_id,source_type,grant_id)
		      VALUES ($1,$2,$3,$4,$5,NULL,$6,'subscription',$7)`, standingEnt, merchantID, cust, feat, subStart, activeSub, activeGrant)
		// OLD cancelled sub, same product/feature, historical period, NO grant —
		// the doujins dual-history import shape.
		exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,started_at,current_period_starts_at,current_period_ends_at,cancelled_at,cancel_type,ended_at)
		      VALUES ($1,$2,$3,$4,$5,'cancelled','ccbill',$6,$7,$7,$8,$8,'user',$8)`,
			cancelledSub, merchantID, cust, prod, price, "flap-old-"+sfx, oldStart, oldEnd)
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key = ANY($2)`,
				merchantID, []string{"subscription:" + activeSub.String(), "subscription:" + cancelledSub.String()})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2`, merchantID, cust)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2`, merchantID, cust)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=ANY($1)`, []uuid.UUID{activeSub, cancelledSub})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, price)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, prod)
			return nil
		})
	})

	accessUnchanged := func(ctx context.Context) {
		t.Helper()
		var endAt, revokedAt *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT end_at, revoked_at FROM openrails.entitlements WHERE id=$1`, standingEnt).Scan(&endAt, &revokedAt))
		require.Nil(t, endAt, "standing window stays open-ended")
		require.Nil(t, revokedAt, "standing window never revoked")
		var entN int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2`, merchantID, cust).Scan(&entN))
		require.Equal(t, 1, entN, "no bounded window added — the standing window is the only projection")
	}

	// Sweep 1: ONE auto-fixed derive.subscription.missing — the cancelled sub's
	// grant is recorded with its frozen historical window, NO window materializes.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &cust})
		require.NoError(t, err)
		require.Equal(t, 1, res.Findings, "only the ungranted cancelled sub is flagged")
		require.Equal(t, 1, res.AutoFixed)
		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type='derive.subscription.missing' AND subject_key=$2`,
			merchantID, "subscription:"+cancelledSub.String()).Scan(&status))
		require.Equal(t, "auto_fixed", status)
		var gStart, gEnd time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT starts_at, ends_at FROM openrails.grants WHERE merchant_id=$1 AND source_type='subscription' AND source_id=$2 AND event='grant'`,
			merchantID, cancelledSub.String()).Scan(&gStart, &gEnd))
		require.WithinDuration(t, oldStart, gStart, time.Second, "grant freezes the historical period")
		require.WithinDuration(t, oldEnd, gEnd, time.Second)
		accessUnchanged(ctx)
		return nil
	}))

	// Sweeps 2..3: ZERO findings — the flap is dead. grant_effect.missing treats
	// the window-less grant as deliberately absent-by-overlap.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		for i := 0; i < 2; i++ {
			res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &cust})
			require.NoError(t, err)
			require.Zero(t, res.Findings, "repeat sweep emits zero findings (no flap)")
		}
		gl := grants.New(appDB.Gen(ctx), merchantID)
		missing, err := gl.MissingEffects(ctx, &cust)
		require.NoError(t, err)
		require.Empty(t, missing, "grant_effect.missing stays quiet on the window-less grant")
		accessUnchanged(ctx)
		return nil
	}))
}

// #695 flap regression, wallet shape: same standing-window customer, but the
// overlapped historical source is a completed solana wallet payment with a
// stored expiration window. Sweep 1 records the purchase grant (payment-linked,
// no window), sweep 2 is findings-free — including derive.grant.missing, which
// the recorded grant now satisfies.
func TestConverge_DeriveFlap_StandingWindowPlusWalletPayment(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	sfx := uuid.NewString()[:8]
	feat := "wflap-feat-" + sfx

	subProd, subPrice, oneOffProd, oneOffPrice := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	activeSub, activeGrant, standingEnt, pay := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	var cust uuid.UUID
	now := time.Now().UTC()
	subStart := now.Add(-130 * 24 * time.Hour)
	periodStart := now.Add(-10 * 24 * time.Hour)
	periodEnd := now.Add(20 * 24 * time.Hour)
	purchased := now.Add(-90 * 24 * time.Hour) // window fully inside the standing one
	expires := now.Add(-60 * 24 * time.Hour)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		cust = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id) VALUES ($1,$2,$2,$3,$4)`,
			subProd, "wflap-sub-"+sfx, []byte(`{"`+feat+`": null}`), merchantID)
		exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,access_duration_hours,auto_renew,merchant_id) VALUES ($1,$2,5000000,'usd',720,true,$3)`,
			subPrice, subProd, merchantID)
		exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,started_at,current_period_starts_at,current_period_ends_at)
		      VALUES ($1,$2,$3,$4,$5,'active','ccbill',$6,$7,$8,$9)`,
			activeSub, merchantID, cust, subProd, subPrice, "wflap-live-"+sfx, subStart, periodStart, periodEnd)
		exec(`INSERT INTO openrails.grants (id,merchant_id,customer_id,kind,source_type,source_id,event,spec_snapshot,starts_at,ends_at)
		      VALUES ($1,$2,$3,'entitlement','subscription',$4,'grant',$5,$6,$7)`,
			activeGrant, merchantID, cust, activeSub.String(), []byte(`{"entitlements":["`+feat+`"]}`), periodStart, periodEnd)
		exec(`INSERT INTO openrails.entitlements (id,merchant_id,customer_id,entitlement,start_at,end_at,source_id,source_type,grant_id)
		      VALUES ($1,$2,$3,$4,$5,NULL,$6,'subscription',$7)`, standingEnt, merchantID, cust, feat, subStart, activeSub, activeGrant)
		// Historical solana wallet payment for a one-off product granting the SAME
		// feature; its stored expiration window sits inside the standing window.
		exec(`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id) VALUES ($1,$2,$2,$3,$4)`,
			oneOffProd, "wflap-oneoff-"+sfx, []byte(`{"`+feat+`": null}`), merchantID)
		exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,merchant_id) VALUES ($1,$2,5000000,'usd',$3)`,
			oneOffPrice, oneOffProd, merchantID)
		exec(`INSERT INTO openrails.payments (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, metadata)
		      VALUES ($1,$2,$3,$4,'solana',$5,5000000,5000000,'usd','completed',$6,$7)`,
			pay, merchantID, cust, oneOffPrice, "wflap-"+sfx, purchased,
			`{"expiration_rfc3339":"`+expires.Format(time.RFC3339)+`"}`)
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key = ANY($2)`,
				merchantID, []string{"subscription:" + activeSub.String(), "payment:" + pay.String()})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2`, merchantID, cust)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2`, merchantID, cust)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE id=$1`, pay)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, activeSub)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=ANY($1)`, []uuid.UUID{subPrice, oneOffPrice})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=ANY($1)`, []uuid.UUID{subProd, oneOffProd})
			return nil
		})
	})

	// Sweep 1: derive.wallet.missing auto-fixes (grant recorded, payment-linked,
	// NO window); derive.grant.missing surfaces once alongside it (detections ran
	// before the repair recorded the grant).
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &cust})
		require.NoError(t, err)
		require.Equal(t, 1, res.AutoFixed, "derive.wallet.missing auto-fixed")
		var gStart, gEnd time.Time
		var paymentID uuid.UUID
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT starts_at, ends_at, payment_id FROM openrails.grants WHERE merchant_id=$1 AND source_type='purchase' AND source_id=$2 AND event='grant'`,
			merchantID, pay.String()).Scan(&gStart, &gEnd, &paymentID))
		require.WithinDuration(t, purchased, gStart, time.Second, "grant freezes the stored wallet window")
		require.WithinDuration(t, expires, gEnd, time.Second)
		require.Equal(t, pay, paymentID, "payment-linked so the refund check sees it")
		var entN int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2`, merchantID, cust).Scan(&entN))
		require.Equal(t, 1, entN, "no window materialized — the standing window is the only projection")
		return nil
	}))

	// Sweep 2: ZERO findings — wallet flap dead, grant.missing satisfied by the
	// recorded grant, grant_effect.missing quiet on the window-less grant.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &cust})
		require.NoError(t, err)
		require.Zero(t, res.Findings, "repeat sweep emits zero findings (no flap)")
		var endAt *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT end_at FROM openrails.entitlements WHERE id=$1`, standingEnt).Scan(&endAt))
		require.Nil(t, endAt, "standing window intact")
		return nil
	}))
}

// #695 unregressed normal path: a NON-overlapped cancelled sub (no other windows
// for the customer) still derives grant + BOUNDED window in one sweep, and the
// second sweep is findings-free.
func TestConverge_DeriveGrantMissing_CancelledSubNonOverlapped(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	sfx := uuid.NewString()[:8]
	feat := "nov-feat-" + sfx

	prod, price, sub := uuid.New(), uuid.New(), uuid.New()
	var cust uuid.UUID
	now := time.Now().UTC()
	oldStart := now.Add(-100 * 24 * time.Hour)
	oldEnd := now.Add(-70 * 24 * time.Hour)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		cust = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id) VALUES ($1,$2,$2,$3,$4)`,
			prod, "nov-prod-"+sfx, []byte(`{"`+feat+`": null}`), merchantID)
		exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,access_duration_hours,auto_renew,merchant_id) VALUES ($1,$2,5000000,'usd',720,true,$3)`,
			price, prod, merchantID)
		exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,started_at,current_period_starts_at,current_period_ends_at,cancelled_at,cancel_type,ended_at)
		      VALUES ($1,$2,$3,$4,$5,'cancelled','ccbill',$6,$7,$7,$8,$8,'user',$8)`,
			sub, merchantID, cust, prod, price, "nov-"+sfx, oldStart, oldEnd)
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "subscription:"+sub.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2`, merchantID, cust)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2`, merchantID, cust)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, sub)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, price)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, prod)
			return nil
		})
	})

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &cust})
		require.NoError(t, err)
		require.Equal(t, 1, res.AutoFixed)
		var entStart, entEnd time.Time
		var srcType string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT start_at, end_at, source_type FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2 AND entitlement=$3 AND revoked_at IS NULL`,
			merchantID, cust, feat).Scan(&entStart, &entEnd, &srcType))
		require.WithinDuration(t, oldStart, entStart, time.Second, "bounded window materialized")
		require.WithinDuration(t, oldEnd, entEnd, time.Second)
		require.Equal(t, "subscription", srcType)
		return nil
	}))

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &cust})
		require.NoError(t, err)
		require.Zero(t, res.Findings, "second sweep is findings-free")
		return nil
	}))
}

// #631 derive-1 wallet path: a completed solana payment carrying a stored
// expiration window for a grantable product is auto-derived into a grant
// (source=purchase → one_off entitlement) + window [purchased_at, expiration).
func TestConverge_DeriveGrantMissing_WalletPayment(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	sfx := uuid.NewString()[:8]

	prod := uuid.New()
	price := uuid.New()
	pay := uuid.New()
	var customer uuid.UUID
	purchased := time.Now().UTC().Add(-100 * time.Hour)
	expires := time.Now().UTC().Add(620 * time.Hour)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, entitlements_spec, merchant_id) VALUES ($1,$2,$2,'{"premium":null}'::jsonb,$3)`, prod, "d1w-"+sfx, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id) VALUES ($1,$2,5000000,'usd',$3)`, price, prod, merchantID)
		exec(`INSERT INTO openrails.payments (id, merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, metadata)
		      VALUES ($1,$2,$3,$4,'solana',$5,5000000,5000000,'usd','completed',$6,$7)`,
			pay, merchantID, customer, price, "w-"+sfx, purchased,
			`{"expiration_rfc3339":"`+expires.Format(time.RFC3339)+`"}`)
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "payment:"+pay.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE id=$1`, pay)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, price)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, prod)
			return nil
		})
	})

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		var entStart, entEnd time.Time
		var srcType string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT start_at, end_at, source_type FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2 AND entitlement='premium' AND revoked_at IS NULL`,
			merchantID, customer).Scan(&entStart, &entEnd, &srcType))
		require.WithinDuration(t, purchased, entStart, time.Second)
		require.WithinDuration(t, expires, entEnd, time.Second)
		require.Equal(t, "one_off", srcType, "purchase-sourced grant materializes as one_off entitlement")
		return nil
	}))
}

// #636 admin comp as source-of-truth: GrantAdmin records an admin-sourced
// entitlement grant + materializes its entitlement (source_type=admin),
// idempotent by sourceID, and supports an indefinite (nil-end) window.
func TestGrantAdmin_MaterializesEntitlement(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	var custBounded, custIndef uuid.UUID
	start := time.Now().UTC().Add(-240 * time.Hour)
	end := time.Now().UTC().Add(480 * time.Hour)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		custBounded = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		custIndef = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			for _, c := range []uuid.UUID{custBounded, custIndef} {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2`, merchantID, c)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2`, merchantID, c)
			}
			return nil
		})
	})

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		gl := grants.New(appDB.Gen(ctx), merchantID)

		// Bounded comp.
		created, existed, err := gl.GrantAdmin(ctx, custBounded, "comp-bounded", []string{"premium"}, start, &end)
		require.NoError(t, err)
		require.Equal(t, 1, created, "first import creates the grant")
		require.False(t, existed)

		var n int
		var srcType string
		var endAt *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2 AND entitlement='premium' AND revoked_at IS NULL`,
			merchantID, custBounded).Scan(&n))
		require.Equal(t, 1, n, "admin comp → one entitlement")
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT source_type, end_at FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2 AND entitlement='premium' AND revoked_at IS NULL`,
			merchantID, custBounded).Scan(&srcType, &endAt))
		require.Equal(t, "admin", srcType)
		require.NotNil(t, endAt)

		// Idempotent re-run.
		created, existed, err = gl.GrantAdmin(ctx, custBounded, "comp-bounded", []string{"premium"}, start, &end)
		require.NoError(t, err)
		require.Equal(t, 0, created)
		require.True(t, existed, "same sourceID → skipped")
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2 AND entitlement='premium' AND revoked_at IS NULL`,
			merchantID, custBounded).Scan(&n))
		require.Equal(t, 1, n, "re-run is a no-op")

		// Indefinite comp (nil end → end_at NULL).
		created, _, err = gl.GrantAdmin(ctx, custIndef, "comp-indef", []string{"premium"}, start, nil)
		require.NoError(t, err)
		require.Equal(t, 1, created)
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT end_at FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2 AND entitlement='premium' AND revoked_at IS NULL`,
			merchantID, custIndef).Scan(&endAt))
		require.Nil(t, endAt, "indefinite comp → open-ended entitlement")

		// #695 blocked comp: window fully inside the existing live window — the
		// grant is STILL recorded (provenance) with created=0 (no window), and a
		// re-run is an idempotent sourceID skip.
		insideEnd := end.Add(-24 * time.Hour)
		created, existed, err = gl.GrantAdmin(ctx, custBounded, "comp-blocked", []string{"premium"}, start.Add(24*time.Hour), &insideEnd)
		require.NoError(t, err)
		require.Equal(t, 0, created, "overlapped comp materializes no window")
		require.False(t, existed)
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2 AND source_type='admin' AND source_id='comp-blocked' AND event='grant'`,
			merchantID, custBounded).Scan(&n))
		require.Equal(t, 1, n, "blocked comp still recorded on the grant ledger (#695)")
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2 AND entitlement='premium' AND revoked_at IS NULL`,
			merchantID, custBounded).Scan(&n))
		require.Equal(t, 1, n, "no second window (constraint never tripped)")
		created, existed, err = gl.GrantAdmin(ctx, custBounded, "comp-blocked", []string{"premium"}, start.Add(24*time.Hour), &insideEnd)
		require.NoError(t, err)
		require.Equal(t, 0, created)
		require.True(t, existed, "re-run is an idempotent skip")
		return nil
	}))
}
