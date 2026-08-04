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

// #665: derive.grant_effect.mismatch (grant direction) — moved from the legacy
// pull engine's PS-9. An ACTIVE sub in a running period whose product promises
// a feature that was never projected for this period (its only grant covers an
// old period) is AUTO-repaired by derive-1: a new grant + live window for
// exactly the missing feature. Idempotent.
func TestConverge_DeriveGrantEffectMismatch_GrantDirection(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	suffix := uuid.NewString()[:8]
	feature := "feat-gd-" + suffix
	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
	var customer uuid.UUID
	now := time.Now().UTC()

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id)
		      VALUES ($1,$2,$2,$3, jsonb_build_object($4::text, null), $5)`,
			productID, "gd-prod-"+suffix, "gd-tier-"+suffix, feature, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
		      VALUES ($1,$2,9990000,'USD',720,true,$3)`, priceID, productID, merchantID)
		// Active sub, RUNNING period [now-5d, now+25d).
		exec(`INSERT INTO openrails.subscriptions
		        (id, price_id, product_id, status, rail, rail_subscription_id,
		         current_period_starts_at, current_period_ends_at, started_at,
		         entitlements_spec_snapshot, customer_id, merchant_id)
		      VALUES ($1,$2,$3,'active','nmi',$4,$5,$6,$7, jsonb_build_object($8::text, null), $9,$10)`,
			subID, priceID, productID, "gd-sub-"+suffix,
			now.Add(-5*24*time.Hour), now.Add(25*24*time.Hour), now.Add(-40*24*time.Hour), feature, customer, merchantID)

		// The sub HAS a grant — but only for a PAST period, with the LEGACY
		// bounded projection (so derive.subscription.missing and
		// grant_effect.missing stay quiet). Seeded directly: since #691,
		// MaterializeGrant would ENSURE a standing window for this live
		// auto-renew sub and the mismatch could never exist — this is the
		// pre-inversion shape migration 063 skips (lapsed window).
		gl := grants.New(appDB.Gen(ctx), merchantID)
		oldEnd := now.Add(-10 * 24 * time.Hour)
		g, err := gl.Grant(ctx, grants.GrantInput{
			Customer: customer, Kind: grants.Entitlement, Source: grants.Subscription,
			SourceID: subID.String(), Spec: &grants.Spec{Entitlements: []string{feature}},
			StartsAt: now.Add(-40 * 24 * time.Hour), EndsAt: &oldEnd,
		})
		require.NoError(t, err)
		exec(`INSERT INTO openrails.entitlements (id, merchant_id, customer_id, entitlement, start_at, end_at, source_id, source_type, grant_id)
		      VALUES ($1,$2,$3,$4,$5,$6,$7,'subscription',$8)`,
			uuid.New(), merchantID, customer, feature, now.Add(-40*24*time.Hour), oldEnd, subID, g.ID)
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "subscription:"+subID.String())
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE customer_id=$1`, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE customer_id=$1`, customer)
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
		require.Equal(t, 1, res.AutoFixed, "grant-direction mismatch is AUTO-repaired via derive-1")

		var status string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND finding_type='derive.grant_effect.mismatch' AND subject_key=$2`,
			merchantID, "subscription:"+subID.String()).Scan(&status))
		require.Equal(t, "auto_fixed", status)

		// A live window for the current period exists now, via a NEW grant.
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements
			 WHERE customer_id=$1 AND entitlement=$2 AND revoked_at IS NULL AND deleted_at IS NULL
			   AND start_at <= now() AND (end_at IS NULL OR end_at > now()) AND grant_id IS NOT NULL`,
			customer, feature).Scan(&n))
		require.Equal(t, 1, n, "derive-1 projected the missing current-period window")
		return nil
	}))

	// Idempotent: the projection exists -> no finding, no writes.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Zero(t, res.Findings, "converged: second run is a no-op")
		return nil
	}))
}

// #665: derive.grant_effect.mismatch (revoke direction) — a terminally-dead
// sub still projecting live subscription-sourced windows is AUTO-repaired
// (windows revoked; backing grants terminated). An `unknown` sub keeps its
// access untouched (#664). Idempotent.
func TestConverge_DeriveGrantEffectMismatch_RevokeDirection(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	e := NewConvergeEngine(appDB)
	suffix := uuid.NewString()[:8]
	productID, priceID := uuid.New(), uuid.New()
	deadSub, unknownSub := uuid.New(), uuid.New()
	deadEnt, unknownEnt := uuid.New(), uuid.New()
	var customer uuid.UUID
	now := time.Now().UTC()

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		// Empty entitlements_spec: keeps derive.subscription.missing quiet — the
		// revoke direction keys on the live windows alone.
		exec(`INSERT INTO openrails.products (id, key, display_name, tier_group, entitlements_spec, merchant_id)
		      VALUES ($1,$2,$2,$3,'{}'::jsonb,$4)`, productID, "rd-prod-"+suffix, "rd-tier-"+suffix, merchantID)
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
		      VALUES ($1,$2,9990000,'USD',720,true,$3)`, priceID, productID, merchantID)
		seedSub := func(id uuid.UUID, status, railSubID string) {
			exec(`INSERT INTO openrails.subscriptions
			        (id, price_id, product_id, status, rail, rail_subscription_id,
			         current_period_starts_at, current_period_ends_at, started_at,
			         entitlements_spec_snapshot, customer_id, merchant_id, cancelled_at, cancel_type, ended_at)
			      VALUES ($1,$2,$3,$4,'nmi',$5,$6,$7,$8,'{}'::jsonb,$9,$10,$11,$12,$13)`,
				id, priceID, productID, status, railSubID,
				now.Add(-40*24*time.Hour), now.Add(-10*24*time.Hour), now.Add(-40*24*time.Hour), customer, merchantID,
				timePtrOrNil(status == "cancelled", now.Add(-10*24*time.Hour)),
				strPtrOrNil(status == "cancelled", "user"),
				timePtrOrNil(status == "cancelled", now.Add(-10*24*time.Hour)))
		}
		seedSub(deadSub, "cancelled", "rd-dead-"+suffix)
		seedSub(unknownSub, "unknown", "rd-unknown-"+suffix)
		// Legacy-shaped LIVE windows (no grants) sourced by each sub. The dead
		// sub's window is BOUNDED but overruns its entitled bound (#690: the
		// standing-window shape belongs to derive.entitlement.unjustified, ADMIN);
		// the unknown sub keeps a standing window (access intact, #664/#691).
		seedEnt := func(id, subID uuid.UUID, feature string, endAt *time.Time) {
			exec(`INSERT INTO openrails.entitlements (id, customer_id, entitlement, start_at, end_at, source_id, source_type, merchant_id)
			      VALUES ($1,$2,$3,$4,$5,$6,'subscription',$7)`,
				id, customer, feature, now.Add(-40*24*time.Hour), endAt, subID, merchantID)
		}
		overrun := now.Add(20 * 24 * time.Hour)
		seedEnt(deadEnt, deadSub, "rd-feat-dead-"+suffix, &overrun)
		seedEnt(unknownEnt, unknownSub, "rd-feat-unk-"+suffix, nil)
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key = ANY($2)`,
				merchantID, []string{"subscription:" + deadSub.String(), "subscription:" + unknownSub.String(), "customer:" + customer.String()})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.notification_queue WHERE merchant_id=$1 AND customer_id=$2`, merchantID, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE customer_id=$1`, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE customer_id=$1`, customer)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=ANY($1)`, []uuid.UUID{deadSub, unknownSub})
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			return nil
		})
	})

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		// 2 findings: the dead sub's mismatch + the #789 post-repair NOTIFY
		// access-ended row for the window this very run just bounded.
		require.Equal(t, 2, res.Findings, "dead-sub mismatch + access-ended notify; unknown sub untouched")
		require.Equal(t, 2, res.AutoFixed)

		// Repair = the missed #691 closure: the window is BOUNDED at the
		// entitled bound (GREATEST(paid-through, ended_at)), not revoked.
		var endAt, revokedAt *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT end_at, revoked_at FROM openrails.entitlements WHERE id=$1`, deadEnt).Scan(&endAt, &revokedAt))
		require.NotNil(t, endAt)
		require.WithinDuration(t, now.Add(-10*24*time.Hour), *endAt, 2*time.Second, "dead sub's overrun window bounded at ended_at/paid-through")
		require.Nil(t, revokedAt, "closure writes end_at, not a revoke")
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `SELECT revoked_at FROM openrails.entitlements WHERE id=$1`, unknownEnt).Scan(&revokedAt))
		require.Nil(t, revokedAt, "unknown sub keeps access (#664: no revoke on a guess)")
		return nil
	}))

	// Idempotent: nothing live remains on the dead sub.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		require.Zero(t, res.Findings)
		return nil
	}))
}

func timePtrOrNil(cond bool, t time.Time) *time.Time {
	if !cond {
		return nil
	}
	return &t
}

func strPtrOrNil(cond bool, s string) *string {
	if !cond {
		return nil
	}
	return &s
}
