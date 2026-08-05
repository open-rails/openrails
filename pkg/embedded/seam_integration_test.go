//go:build integration

package embedded

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

// End-to-end of the doujins->openrails seam (#631/#636/#637), the faithful proxy
// for the from-scratch legacy migration: seed FACTS the migrate writes (an active
// subscription + a solana wallet payment, with NO grants/entitlements), hand an
// admin comp over via the public ImportAdminGrants, then ConvergeMerchant — and
// assert OpenRails materialized a grant + entitlement for each cohort. Exercises
// the exact public embed API the doujins legacy_migrate calls.
func TestEmbeddedSeam_ImportAdminGrantsThenConverge(t *testing.T) {
	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	appDB, err := db.NewWithPGXPool(pool, "")
	require.NoError(t, err)

	sfx := uuid.NewString()[:8]
	prod, price := uuid.New(), uuid.New()
	sub, pay := uuid.New(), uuid.New()
	adminSource := "e2e-comp-" + sfx
	var custSub, custWallet, custAdmin uuid.UUID
	start := time.Now().UTC().Add(-240 * time.Hour)
	end := time.Now().UTC().Add(480 * time.Hour)
	purchased := time.Now().UTC().Add(-100 * time.Hour)
	expires := time.Now().UTC().Add(620 * time.Hour)

	// Seed source-of-truth facts ONLY (no grants, no entitlements) — as the migrate does.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		custSub = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		custWallet = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		custAdmin = dbtest.EnsureCustomerIDPgx(ctx, t, appDB.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, e := appDB.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, e)
		}
		mobiusPSP := dbtest.EnsureTestPSP(ctx, t, appDB.Qx(ctx), merchantID, "mobius")
		solanaPSP := dbtest.EnsureTestPSP(ctx, t, appDB.Qx(ctx), merchantID, "solana")
		exec(`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id) VALUES ($1,$2,$2,'{"premium":null}'::jsonb,$3)`, prod, "e2e-"+sfx, merchantID)
		exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,merchant_id) VALUES ($1,$2,5000000,'USD',$3)`, price, prod, merchantID)
		exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,started_at,current_period_starts_at,current_period_ends_at,psp_id)
		      VALUES ($1,$2,$3,$4,$5,'active','mobius',$6,$6,$7,$8)`, sub, merchantID, custSub, prod, price, start, end, mobiusPSP)
		exec(`INSERT INTO openrails.payments (id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,purchased_at,metadata,psp_id)
		      VALUES ($1,$2,$3,$4,'solana',$5,5000000,5000000,'USD','completed',$6,$7,$8)`,
			pay, merchantID, custWallet, price, "e2e-w-"+sfx, purchased,
			`{"expiration_rfc3339":"`+expires.Format(time.RFC3339)+`"}`, solanaPSP)
		return nil
	}))

	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			for _, c := range []uuid.UUID{custSub, custWallet, custAdmin} {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2`, merchantID, c)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2`, merchantID, c)
			}
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE id=$1`, sub)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE id=$1`, pay)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, price)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, prod)
			return nil
		})
	})

	// (1) Admin comp handed over as a grant via the public seam (Config nil ->
	// DefaultSchema, identical to the doujins embed wiring).
	res, err := ImportAdminGrants(context.Background(), AdminGrantImportOptions{
		PGXPool:      pool,
		MerchantSlug: dbtest.TestMerchantSlug,
		Grants:       []AdminGrant{{Customer: custAdmin, Product: prod, SourceID: adminSource, StartsAt: start, EndsAt: &end}},
	})
	require.NoError(t, err)
	require.Equal(t, []string{adminSource}, res.Imported, "admin comp imported as a grant")

	// (2) Merchant-wide convergence derives the subscription + wallet grants.
	_, err = ConvergeMerchant(context.Background(), ConvergeMerchantOptions{
		PGXPool:      pool,
		MerchantSlug: dbtest.TestMerchantSlug,
	})
	require.NoError(t, err)

	// (3) Every cohort now traces to a grant -> entitlement, with the right source.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		entWithSource := func(c uuid.UUID) (int, string) {
			var n int
			var src string
			require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
				`SELECT count(*), COALESCE(max(source_type),'') FROM openrails.entitlements
				 WHERE merchant_id=$1 AND customer_id=$2 AND entitlement='premium' AND revoked_at IS NULL AND deleted_at IS NULL`,
				merchantID, c).Scan(&n, &src))
			return n, src
		}
		grantCount := func(c uuid.UUID) int {
			var n int
			require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
				`SELECT count(*) FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2 AND event='grant'`,
				merchantID, c).Scan(&n))
			return n
		}

		n, src := entWithSource(custSub)
		require.Equal(t, 1, n, "subscription cohort: one entitlement")
		require.Equal(t, "subscription", src)
		require.Equal(t, 1, grantCount(custSub))

		n, src = entWithSource(custWallet)
		require.Equal(t, 1, n, "wallet cohort: one entitlement")
		require.Equal(t, "one_off", src)
		require.Equal(t, 1, grantCount(custWallet))

		n, src = entWithSource(custAdmin)
		require.Equal(t, 1, n, "admin cohort: one entitlement")
		require.Equal(t, "admin", src)
		require.Equal(t, 1, grantCount(custAdmin))
		return nil
	}))
}
