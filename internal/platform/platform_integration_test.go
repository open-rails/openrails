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

	"github.com/open-rails/openrails/pkg/merchant"
)

const schemaDDL = `
CREATE SCHEMA IF NOT EXISTS openrails;

CREATE TABLE IF NOT EXISTS openrails.tenants (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug         TEXT NOT NULL UNIQUE,
    name         TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active',
    billing_tier TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    deleted_at   TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS openrails.subscriptions (
    id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID,
    status    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS openrails.payments (
    id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID,
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

func seedTenant(t *testing.T, pool *pgxpool.Pool, slug, status, tier string) merchant.ID {
	t.Helper()
	var idStr string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO openrails.tenants (slug, name, status, billing_tier)
		VALUES ($1, $1, $2, NULLIF($3,'')) RETURNING id::text
	`, slug, status, tier).Scan(&idStr)
	require.NoError(t, err)
	id, err := merchant.ParseID(idStr)
	require.NoError(t, err)
	return id
}

func TestMetrics_AggregatesAcrossTenants(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	acme := seedTenant(t, pool, "acme", "active", "pro")
	globex := seedTenant(t, pool, "globex", "suspended", "free")
	_ = seedTenant(t, pool, "empty", "active", "")

	exec := func(q string, args ...any) {
		_, err := pool.Exec(ctx, q, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO openrails.subscriptions (tenant_id, status) VALUES ($1::uuid,'active'),($1::uuid,'active'),($1::uuid,'failed')`, acme.String())
	exec(`INSERT INTO openrails.payments (tenant_id, amount, status) VALUES ($1::uuid,500,'completed'),($1::uuid,1500,'completed'),($1::uuid,999,'refunded')`, acme.String())
	exec(`INSERT INTO openrails.subscriptions (tenant_id, status) VALUES ($1::uuid,'active')`, globex.String())
	exec(`INSERT INTO openrails.payments (tenant_id, amount, status) VALUES ($1::uuid,300,'completed')`, globex.String())

	metrics, err := NewMetrics(pool)
	require.NoError(t, err)
	m, err := metrics.Compute(ctx)
	require.NoError(t, err)

	require.Equal(t, 3, m.TenantCount)
	require.Equal(t, 2, m.ActiveTenantCount)
	require.Equal(t, 1, m.SuspendedTenantCount)
	require.Equal(t, int64(3), m.TotalActiveSubs)
	require.Equal(t, int64(500+1500+300), m.TotalRevenueMinor)
	require.Equal(t, int64(1), m.TotalWebhookFailures)

	byTenant := map[string]TenantMetric{}
	for _, tm := range m.Tenants {
		byTenant[tm.Slug] = tm
	}
	require.Equal(t, int64(2), byTenant["acme"].ActiveSubs)
	require.Equal(t, int64(2000), byTenant["acme"].RevenueMinor)
	require.Equal(t, int64(1), byTenant["acme"].WebhookFailures)
	require.Equal(t, int64(1), byTenant["globex"].ActiveSubs)
	require.Equal(t, int64(300), byTenant["globex"].RevenueMinor)
	require.Equal(t, "free", byTenant["globex"].BillingTier)
	require.Zero(t, byTenant["empty"].ActiveSubs)
}
