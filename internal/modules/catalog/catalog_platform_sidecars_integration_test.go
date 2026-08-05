//go:build integration

package catalog

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// startFeatureRLSPostgres boots Postgres, applies all billing migrations
// (including 050 RLS + the openrails_app role), and returns a *db.DB connected
// AS the unprivileged openrails_app role — the only role for which RLS actually
// enforces. (Same scaffolding as the entitlements-module RLS tests.)
func startFeatureRLSPostgres(t *testing.T) (appDB *db.DB, ctx context.Context) {
	t.Helper()
	ctx = context.Background()
	_, appDSN := dbtest.SharedRLSPostgres(t)

	pool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	appDB, err = db.NewWithPGXPool(pool, "") // default schema (shared harness)
	require.NoError(t, err)
	return appDB, ctx
}

// seedTenantAndProduct inserts a merchant + one product as the app role inside
// the correct merchant tx.
func seedTenantAndProduct(t *testing.T, ctx context.Context, appDB *db.DB, tid merchant.ID, productID uuid.UUID, productKey string) {
	t.Helper()
	require.NoError(t, appDB.MerchantTx(merchant.WithID(ctx, tid), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active') ON CONFLICT (id) DO NOTHING`,
			tid.UUID(), "t-"+tid.String()[:8],
		); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO openrails.products (id, merchant_id, key, display_name) VALUES ($1, $2, $3, $3)`,
			productID, tid.UUID(), productKey,
		)
		return err
	}))
}

func TestCatalogBenefitAndMeteringSidecars_AppRoleRLS(t *testing.T) {
	appDB, ctx := startFeatureRLSPostgres(t)

	tA := merchant.ID(uuid.New())
	tB := merchant.ID(uuid.New())
	productA := uuid.New()
	productB := uuid.New()
	priceA := uuid.New()
	priceB := uuid.New()
	customerA := uuid.New()
	suffix := uuid.NewString()[:8]
	usageKey := "api-limit-" + suffix
	meterKey := "api-calls-" + suffix

	seedTenantAndProduct(t, ctx, appDB, tA, productA, "prod-a-"+suffix)
	seedTenantAndProduct(t, ctx, appDB, tB, productB, "prod-b-"+suffix)

	ctxA := merchant.WithID(ctx, tA)
	ctxB := merchant.WithID(ctx, tB)

	require.NoError(t, appDB.MerchantTx(ctxA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1, $2, $3)`,
			customerA, tA.UUID(), customerA.String(),
		)
		require.NoError(t, err)
		_, err = tx.Exec(ctx,
			`INSERT INTO openrails.prices (id, product_id, merchant_id, amount, currency, access_duration_hours, auto_renew)
			 VALUES ($1, $2, $3, 1250000, 'USD', 720, true)`,
			priceA, productA, tA.UUID(),
		)
		require.NoError(t, err)
		_, err = tx.Exec(ctx,
			`INSERT INTO openrails.catalog_usage_limits (merchant_id, key, measure, windows)
			 VALUES ($1, $2, 'api_requests', '[{"period":"day","limit":1000}]'::jsonb)`,
			tA.UUID(), usageKey,
		)
		require.NoError(t, err)
		_, err = tx.Exec(ctx,
			`INSERT INTO openrails.product_usage_limit_bindings
			 (id, merchant_id, customer_id, usage_limit_key, measure, windows)
			 VALUES ($1, $2, $3, $4, 'api_requests', '[{"period":"day","limit":1000}]'::jsonb)`,
			uuid.New(), tA.UUID(), customerA, usageKey,
		)
		require.NoError(t, err)
		_, err = tx.Exec(ctx,
			`INSERT INTO openrails.catalog_meters (merchant_id, key, kind) VALUES ($1, $2, 'counter')`,
			tA.UUID(), meterKey,
		)
		require.NoError(t, err)
		_, err = tx.Exec(ctx,
			`INSERT INTO openrails.catalog_rate_cards (merchant_id, product_id, ordinal, meter_key, payment_term, price)
			 VALUES ($1, $2, 1, $3, 'in_arrears', '{"model":"per_unit","per_unit":{"unit_amount":100000,"divide_by":100}}'::jsonb)`,
			tA.UUID(), productA, meterKey,
		)
		return err
	}))

	require.NoError(t, appDB.MerchantTx(ctxA, func(ctx context.Context, tx pgx.Tx) error {
		var usageCount, bindingCount, meterCount, rateCardCount int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.catalog_usage_limits WHERE key = $1`, usageKey).Scan(&usageCount))
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.product_usage_limit_bindings WHERE usage_limit_key = $1`, usageKey).Scan(&bindingCount))
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.catalog_meters WHERE key = $1`, meterKey).Scan(&meterCount))
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.catalog_rate_cards WHERE meter_key = $1`, meterKey).Scan(&rateCardCount))
		require.Equal(t, 1, usageCount)
		require.Equal(t, 1, bindingCount)
		require.Equal(t, 1, meterCount)
		require.Equal(t, 1, rateCardCount)
		return nil
	}))

	require.NoError(t, appDB.MerchantTx(ctxB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO openrails.prices (id, product_id, merchant_id, amount, currency, access_duration_hours, auto_renew)
			 VALUES ($1, $2, $3, 1500000, 'USD', 720, true)`,
			priceB, productB, tB.UUID(),
		)
		require.NoError(t, err)

		var usageCount, meterCount, rateCardCount int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.catalog_usage_limits WHERE key = $1`, usageKey).Scan(&usageCount))
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.catalog_meters WHERE key = $1`, meterKey).Scan(&meterCount))
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.catalog_rate_cards WHERE meter_key = $1`, meterKey).Scan(&rateCardCount))
		require.Equal(t, 0, usageCount)
		require.Equal(t, 0, meterCount)
		require.Equal(t, 0, rateCardCount)
		return nil
	}))

	err := appDB.MerchantTx(ctxB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO openrails.catalog_meters (merchant_id, key, kind) VALUES ($1, $2, 'counter')`,
			tA.UUID(), "cross-tenant-"+suffix,
		)
		return err
	})
	require.Error(t, err, "app role must not write catalog sidecars for another merchant")
}
