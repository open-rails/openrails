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

// #631 derive-1 overlap guard: when the customer already holds a LIVE entitlement
// for the feature whose window overlaps the subscription period, derive-1 SKIPs
// the grant rather than tripping the no-overlap exclusion constraint.
func TestConverge_DeriveSubscription_SkipsOverlap(t *testing.T) {
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

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, err := e.Converge(ctx, Scope{Merchant: dbtest.TestMerchantID, Customer: &customer})
		require.NoError(t, err)
		var grantN int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2 AND source_type='subscription'`,
			merchantID, customer).Scan(&grantN))
		require.Equal(t, 0, grantN, "overlapping window → subscription grant skipped (constraint never tripped)")
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
		created, err := gl.GrantAdmin(ctx, custBounded, "comp-bounded", []string{"premium"}, start, &end)
		require.NoError(t, err)
		require.True(t, created, "first import creates the grant")

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
		created, err = gl.GrantAdmin(ctx, custBounded, "comp-bounded", []string{"premium"}, start, &end)
		require.NoError(t, err)
		require.False(t, created, "same sourceID → skipped")
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2 AND entitlement='premium' AND revoked_at IS NULL`,
			merchantID, custBounded).Scan(&n))
		require.Equal(t, 1, n, "re-run is a no-op")

		// Indefinite comp (nil end → end_at NULL).
		created, err = gl.GrantAdmin(ctx, custIndef, "comp-indef", []string{"premium"}, start, nil)
		require.NoError(t, err)
		require.True(t, created)
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT end_at FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2 AND entitlement='premium' AND revoked_at IS NULL`,
			merchantID, custIndef).Scan(&endAt))
		require.Nil(t, endAt, "indefinite comp → open-ended entitlement")
		return nil
	}))
}
