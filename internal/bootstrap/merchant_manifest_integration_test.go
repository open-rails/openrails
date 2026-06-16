//go:build integration

package bootstrap

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	authcore "github.com/open-rails/authkit/core"
	authpgmigrations "github.com/open-rails/authkit/migrations/postgres"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
)

const merchantManifestSchemaDDL = `
CREATE SCHEMA IF NOT EXISTS openrails;

CREATE TABLE IF NOT EXISTS openrails.merchants (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug                TEXT NOT NULL UNIQUE,
    name                TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'active',
    owner_org_id     TEXT,
    plan                TEXT,
    region              TEXT,
    billing_tier        TEXT,
    stripe_account_id   TEXT,
    webhook_host        TEXT,
    webhook_path        TEXT,
    provisioned_at      TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    suspended_at        TIMESTAMPTZ,
    deleted_at          TIMESTAMPTZ
);
`

func TestReconcileMerchantManifestEnsuresTenants(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	require.NoError(t, ReconcileMerchantManifestData(ctx, &config.Config{}, cp, cozyArtMerchantManifest("starter", "us-west", "/hooks/v1"), MerchantManifestReconcileOptions{}))

	var tenantID, ownerOrgID, billingTier, region, webhookPath string
	require.NoError(t, pool.QueryRow(ctx, `
			SELECT id::text, owner_org_id, billing_tier, region, webhook_path
		  FROM openrails.merchants
		 WHERE slug = 'cozy-art'
	`).Scan(&tenantID, &ownerOrgID, &billingTier, &region, &webhookPath))
	require.Equal(t, "starter", billingTier)
	require.Equal(t, "us-west", region)
	require.Equal(t, "/hooks/v1", webhookPath)

	ownerOrg, err := cp.Core().ResolveOrgBySlug(ctx, "cozy-art")
	require.NoError(t, err)
	require.Equal(t, ownerOrg.ID, ownerOrgID, "manifest bootstrap should bind the merchant namespace to a bootstrap-created owner org")

	require.NoError(t, ReconcileMerchantManifestData(ctx, &config.Config{}, cp, cozyArtMerchantManifest("pro", "us-east", "/hooks/v2"), MerchantManifestReconcileOptions{}))

	require.NoError(t, pool.QueryRow(ctx, `
			SELECT owner_org_id, billing_tier, region, webhook_path
		  FROM openrails.merchants
		 WHERE slug = 'cozy-art'
	`).Scan(&ownerOrgID, &billingTier, &region, &webhookPath))
	require.Equal(t, ownerOrg.ID, ownerOrgID)
	require.Equal(t, "pro", billingTier)
	require.Equal(t, "us-east", region)
	require.Equal(t, "/hooks/v2", webhookPath)
}

func TestReconcileMerchantManifestSerializesConcurrentReplicas(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	manifest := cozyArtMerchantManifest("starter", "us-west", "/hooks/v1")

	start := make(chan struct{})
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := ReconcileMerchantManifestData(ctx, &config.Config{}, cp, manifest, MerchantManifestReconcileOptions{}); err == nil {
				successes.Add(1)
			} else {
				require.NoError(t, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	require.EqualValues(t, 2, successes.Load())

}

func newMerchantManifestTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	if dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN")); dsn != "" {
		pool := newExternalMerchantManifestTestPool(t, ctx, dsn)
		applyMerchantManifestTestSchema(t, ctx, pool)
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

	applyMerchantManifestTestSchema(t, ctx, pool)
	return pool
}

func newExternalMerchantManifestTestPool(t *testing.T, ctx context.Context, adminDSN string) *pgxpool.Pool {
	t.Helper()
	adminCfg, err := pgxpool.ParseConfig(adminDSN)
	require.NoError(t, err)
	adminCfg.ConnConfig.Config.Database = "postgres"
	adminPool, err := pgxpool.NewWithConfig(ctx, adminCfg)
	require.NoError(t, err)

	dbName := fmt.Sprintf("openrails_tenant_manifest_%d", time.Now().UnixNano())
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

func applyMerchantManifestTestSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	entries, err := authpgmigrations.FS.ReadDir(".")
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		b, rerr := authpgmigrations.FS.ReadFile(name)
		require.NoError(t, rerr)
		_, eerr := pool.Exec(ctx, string(b))
		require.NoErrorf(t, eerr, "apply authkit migration %s", name)
	}
	_, err = pool.Exec(ctx, merchantManifestSchemaDDL)
	require.NoError(t, err)
}

func newMerchantManifestControlPlane(t *testing.T, pool *pgxpool.Pool) *controlplane.ControlPlane {
	t.Helper()
	cfg := &config.Config{
		Env: "test",
		Auth: &config.AuthConfig{
			ExpectedAudience: "openrails",
			ControlPlane: &config.ControlPlaneConfig{
				Issuer:      "https://openrails.test",
				TokenPrefix: "openrails",
			},
		},
	}
	cp, err := controlplane.New(context.Background(), cfg, pool)
	require.NoError(t, err)
	require.NotNil(t, cp)
	return cp
}

func cozyArtMerchantManifest(tier, region, webhookPath string) *MerchantManifest {
	return &MerchantManifest{
		Version: BootstrapManifestVersion,
		Merchants: []ManifestMerchant{{
			Slug:        "cozy-art",
			Name:        "Cozy Art",
			BillingTier: tier,
			Region:      region,
			WebhookHost: "cozy.example",
			WebhookPath: webhookPath,
		}},
	}
}

func resourceIDs(resources []authcore.ServiceTokenResource, kind string) []string {
	out := make([]string, 0, len(resources))
	for _, r := range resources {
		if r.Kind == kind {
			out = append(out, r.ID)
		}
	}
	return out
}
