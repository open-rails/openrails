//go:build integration

package repo

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

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
			`INSERT INTO openrails.prices (id, product_id, merchant_id, amount, currency, billing_cycle_days)
			 VALUES ($1, $2, $3, 1250000, 'usd', 30)`,
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
			 (id, merchant_id, customer_id, usage_limit_key, measure, windows, source_type, product_key)
			 VALUES ($1, $2, $3, $4, 'api_requests', '[{"period":"day","limit":1000}]'::jsonb, 'catalog_product', 'pro')`,
			uuid.New(), tA.UUID(), customerA, usageKey,
		)
		require.NoError(t, err)
		_, err = tx.Exec(ctx,
			`INSERT INTO openrails.catalog_meters (merchant_id, key, kind) VALUES ($1, $2, 'counter')`,
			tA.UUID(), meterKey,
		)
		require.NoError(t, err)
		_, err = tx.Exec(ctx,
			`INSERT INTO openrails.catalog_price_metered (price_id, merchant_id, meter_key, rate_micros, per_units)
			 VALUES ($1, $2, $3, 100000, 100)`,
			priceA, tA.UUID(), meterKey,
		)
		return err
	}))

	require.NoError(t, appDB.MerchantTx(ctxA, func(ctx context.Context, tx pgx.Tx) error {
		var usageCount, bindingCount, meterCount, priceMeterCount int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.catalog_usage_limits WHERE key = $1`, usageKey).Scan(&usageCount))
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.product_usage_limit_bindings WHERE usage_limit_key = $1`, usageKey).Scan(&bindingCount))
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.catalog_meters WHERE key = $1`, meterKey).Scan(&meterCount))
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.catalog_price_metered WHERE price_id = $1`, priceA).Scan(&priceMeterCount))
		require.Equal(t, 1, usageCount)
		require.Equal(t, 1, bindingCount)
		require.Equal(t, 1, meterCount)
		require.Equal(t, 1, priceMeterCount)
		return nil
	}))

	require.NoError(t, appDB.MerchantTx(ctxB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO openrails.prices (id, product_id, merchant_id, amount, currency, billing_cycle_days)
			 VALUES ($1, $2, $3, 1500000, 'usd', 30)`,
			priceB, productB, tB.UUID(),
		)
		require.NoError(t, err)

		var usageCount, meterCount, priceMeterCount int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.catalog_usage_limits WHERE key = $1`, usageKey).Scan(&usageCount))
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.catalog_meters WHERE key = $1`, meterKey).Scan(&meterCount))
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.catalog_price_metered WHERE price_id = $1`, priceA).Scan(&priceMeterCount))
		require.Equal(t, 0, usageCount)
		require.Equal(t, 0, meterCount)
		require.Equal(t, 0, priceMeterCount)
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

func TestIdentityAnchorsAndProviderOwner_Integration(t *testing.T) {
	ctx := context.Background()
	superDSN, appDSN := dbtest.SharedRLSPostgres(t)

	super, err := db.NewDB(&config.DBConfig{URL: superDSN})
	require.NoError(t, err)
	defer super.Close()

	customerAnchor := "pg-customer-" + uuid.NewString()
	merchantAnchor := "pg-merchant-" + uuid.NewString()
	_, err = super.Pool().Exec(ctx,
		`INSERT INTO openrails.customer_anchors (permission_group_id) VALUES ($1)
		 ON CONFLICT DO NOTHING`,
		customerAnchor,
	)
	require.NoError(t, err)
	_, err = super.Pool().Exec(ctx,
		`INSERT INTO openrails.merchant_anchors (permission_group_id) VALUES ($1)
		 ON CONFLICT DO NOTHING`,
		merchantAnchor,
	)
	require.NoError(t, err)

	var anchorCount int
	require.NoError(t, super.Pool().QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM openrails.customer_anchors WHERE permission_group_id = $1) +
			(SELECT count(*) FROM openrails.merchant_anchors WHERE permission_group_id = $2)
	`, customerAnchor, merchantAnchor).Scan(&anchorCount))
	require.Equal(t, 2, anchorCount)

	pool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	appDB, err := db.NewWithPGXPool(pool, "")
	require.NoError(t, err)

	tid := merchant.ID(uuid.New())
	ctxTenant := merchant.WithID(ctx, tid)
	accountID := "acct_" + uuid.NewString()[:8]
	require.NoError(t, appDB.MerchantTx(ctxTenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active') ON CONFLICT (id) DO NOTHING`,
			tid.UUID(), "owner-"+tid.String()[:8],
		)
		require.NoError(t, err)
		_, err = tx.Exec(ctx,
			`INSERT INTO openrails.provider_accounts
			 (merchant_id, provider_type, environment, account_id, role, status, owner)
			 VALUES ($1, 'stripe', 'test', $2, 'secondary', 'enabled', 'platform')`,
			tid.UUID(), accountID,
		)
		require.NoError(t, err)

		var owner string
		require.NoError(t, tx.QueryRow(ctx,
			`SELECT owner FROM openrails.provider_accounts WHERE merchant_id = $1 AND account_id = $2`,
			tid.UUID(), accountID,
		).Scan(&owner))
		require.Equal(t, "platform", owner)
		return nil
	}))

	err = appDB.MerchantTx(ctxTenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO openrails.provider_accounts
			 (merchant_id, provider_type, environment, account_id, role, status, owner)
			 VALUES ($1, 'stripe', 'test', $2, 'secondary', 'enabled', 'customer')`,
			tid.UUID(), "acct_bad_"+uuid.NewString()[:8],
		)
		return err
	})
	require.Error(t, err, "provider account owner is limited to merchant/platform")
}
