//go:build integration

package platform

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

const schemaDDL = `
CREATE SCHEMA IF NOT EXISTS openrails;

CREATE TABLE IF NOT EXISTS openrails.merchants (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug         TEXT NOT NULL UNIQUE,
    status       TEXT NOT NULL DEFAULT 'active',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    deleted_at   TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS openrails.subscriptions (
    id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id UUID,
    status    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS openrails.payments (
    id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id UUID,
    amount    BIGINT NOT NULL,
    status    TEXT NOT NULL DEFAULT 'completed'
);
`

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	if dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN")); dsn != "" {
		pool := newExternalPlatformTestPool(t, ctx, dsn)
		_, err := pool.Exec(ctx, schemaDDL)
		require.NoError(t, err)
		return pool
	}

	container, err := postgres.Run(ctx,
		"postgres:18-alpine",
		postgres.WithDatabase("openrails"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		dbtest.WithPostgresLimits(),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, schemaDDL)
	require.NoError(t, err)
	return pool
}

func newExternalPlatformTestPool(t *testing.T, ctx context.Context, adminDSN string) *pgxpool.Pool {
	t.Helper()
	adminCfg, err := pgxpool.ParseConfig(adminDSN)
	require.NoError(t, err)
	adminCfg.ConnConfig.Config.Database = "postgres"
	adminPool, err := pgxpool.NewWithConfig(ctx, adminCfg)
	require.NoError(t, err)

	dbName := fmt.Sprintf("openrails_platform_layer_%d", time.Now().UnixNano())
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize())
	require.NoError(t, err)

	testCfg, err := pgxpool.ParseConfig(adminDSN)
	require.NoError(t, err)
	testCfg.ConnConfig.Config.Database = dbName
	pool, err := pgxpool.NewWithConfig(ctx, testCfg)
	require.NoError(t, err)

	t.Cleanup(func() {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", dbName)
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{dbName}.Sanitize())
		adminPool.Close()
	})
	return pool
}

func seedMerchant(t *testing.T, pool *pgxpool.Pool, slug, status string) merchant.ID {
	t.Helper()
	var idStr string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO openrails.merchants (slug, status)
		VALUES ($1, $2) RETURNING id::text
	`, slug, status).Scan(&idStr)
	require.NoError(t, err)
	id, err := merchant.ParseID(idStr)
	require.NoError(t, err)
	return id
}

func TestMetrics_AggregatesAcrossMerchants(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	acme := seedMerchant(t, pool, "acme", "active")
	globex := seedMerchant(t, pool, "globex", "suspended")
	_ = seedMerchant(t, pool, "empty", "active")

	exec := func(q string, args ...any) {
		_, err := pool.Exec(ctx, q, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO openrails.subscriptions (merchant_id, status) VALUES ($1::uuid,'active'),($1::uuid,'active'),($1::uuid,'failed')`, acme.String())
	exec(`INSERT INTO openrails.payments (merchant_id, amount, status) VALUES ($1::uuid,500,'completed'),($1::uuid,1500,'completed'),($1::uuid,999,'refunded')`, acme.String())
	exec(`INSERT INTO openrails.subscriptions (merchant_id, status) VALUES ($1::uuid,'active')`, globex.String())
	exec(`INSERT INTO openrails.payments (merchant_id, amount, status) VALUES ($1::uuid,300,'completed')`, globex.String())

	metrics, err := NewMetrics(db.WrapPool(pool, ""))
	require.NoError(t, err)
	m, err := metrics.Compute(ctx)
	require.NoError(t, err)

	require.Equal(t, 3, m.MerchantCount)
	require.Equal(t, 2, m.ActiveMerchantCount)
	require.Equal(t, 1, m.SuspendedMerchantCount)
	require.Equal(t, int64(3), m.TotalActiveSubs)
	require.Equal(t, int64(500+1500+300), m.TotalRevenueMinor)
	require.Equal(t, int64(1), m.TotalWebhookFailures)

	byMerchant := map[string]MerchantMetric{}
	for _, tm := range m.Merchants {
		byMerchant[tm.Slug] = tm
	}
	require.Equal(t, int64(2), byMerchant["acme"].ActiveSubs)
	require.Equal(t, int64(2000), byMerchant["acme"].RevenueMinor)
	require.Equal(t, int64(1), byMerchant["acme"].WebhookFailures)
	require.Equal(t, int64(1), byMerchant["globex"].ActiveSubs)
	require.Equal(t, int64(300), byMerchant["globex"].RevenueMinor)
	require.Zero(t, byMerchant["empty"].ActiveSubs)
}
