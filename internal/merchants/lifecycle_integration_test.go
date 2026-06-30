//go:build integration

package merchants

import (
	"context"
	"errors"
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

// schemaDDL stands up the minimal openrails.* schema the merchant service touches:
// the merchant directory (with #225 columns), one representative merchant-owned table
// (entitlements) so export/delete have rows to purge, and the #225 control-plane
// tables. It mirrors migrations 039 + 041 for the columns under test.
const schemaDDL = `
CREATE SCHEMA IF NOT EXISTS openrails;

CREATE TABLE IF NOT EXISTS openrails.merchants (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug             TEXT NOT NULL UNIQUE,
    status           TEXT NOT NULL DEFAULT 'active',
    permission_group_id  TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    deleted_at       TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS openrails.entitlements (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id      UUID,
    customer_id UUID
);

CREATE TABLE IF NOT EXISTS openrails.customers (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id UUID NOT NULL
);

CREATE TABLE IF NOT EXISTS openrails.merchant_secrets (
    merchant_id UUID NOT NULL,
    name       TEXT NOT NULL,
    value      TEXT NOT NULL,
    version    INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    PRIMARY KEY (merchant_id, name)
);

CREATE TABLE IF NOT EXISTS openrails.merchant_credential_audit (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id UUID NOT NULL,
    name       TEXT NOT NULL,
    action     TEXT NOT NULL,
    actor      TEXT,
    detail     TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

CREATE TABLE IF NOT EXISTS openrails.provider_accounts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    merchant_id uuid NOT NULL,
    provider_type text NOT NULL,
    environment text DEFAULT 'live' NOT NULL,
    account_id text NOT NULL,
    routing text DEFAULT 'primary' NOT NULL,
    status text DEFAULT 'enabled' NOT NULL,
    PRIMARY KEY (id),
    UNIQUE (merchant_id, provider_type, environment, account_id)
);

CREATE TABLE IF NOT EXISTS openrails.merchant_exports (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id  UUID NOT NULL,
    status       TEXT NOT NULL DEFAULT 'completed',
    location     TEXT,
    row_counts   JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    completed_at TIMESTAMPTZ
);
`

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	if dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN")); dsn != "" {
		pool := newExternalTenancyTestPool(t, ctx, dsn)
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

func newExternalTenancyTestPool(t *testing.T, ctx context.Context, adminDSN string) *pgxpool.Pool {
	t.Helper()
	adminCfg, err := pgxpool.ParseConfig(adminDSN)
	require.NoError(t, err)
	adminCfg.ConnConfig.Config.Database = "postgres"
	adminPool, err := pgxpool.NewWithConfig(ctx, adminCfg)
	require.NoError(t, err)

	dbName := fmt.Sprintf("openrails_tenancy_%d", time.Now().UnixNano())
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

func newSvc(t *testing.T) *Service {
	pool := newTestPool(t)
	svc, err := NewService(db.WrapPool(pool, ""), NewMemorySecretStore())
	require.NoError(t, err)
	return svc
}

func seedProviderAccount(t *testing.T, svc *Service, merchantID merchant.ID, providerType, environment, accountID string) {
	t.Helper()
	_, err := svc.pool.Exec(context.Background(), `
		INSERT INTO openrails.provider_accounts (merchant_id, provider_type, environment, account_id, routing, status)
		VALUES ($1::uuid, lower($2), $3, $4, 'primary', 'enabled')
		ON CONFLICT (merchant_id, provider_type, environment, account_id) DO NOTHING
	`, merchantID.String(), providerType, environment, accountID)
	require.NoError(t, err)
}

func TestProvision_Idempotent(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t)

	// #500: provisioning a merchant namespace requires the caller to provide the
	// durable AuthKit permission group id. The lifecycle service does not mint it.
	req := ProvisionRequest{Slug: "acme", PermissionGroupID: "group-acme"}
	first, err := svc.Provision(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "acme", first.Slug)
	require.Equal(t, "group-acme", first.PermissionGroupID, "explicit permission-group link recorded (never auto-minted)")

	// Re-provision: same merchant id, no duplicate row (idempotent).
	second, err := svc.Provision(ctx, req)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)

	var count int
	require.NoError(t, svc.pool.QueryRow(ctx, `SELECT count(*) FROM openrails.merchants WHERE slug='acme'`).Scan(&count))
	require.Equal(t, 1, count, "provision must not create a duplicate merchant row")

	_, err = svc.Provision(ctx, ProvisionRequest{Slug: "noown"})
	require.ErrorIs(t, err, ErrPermissionGroupRequired, "control-plane provisioning must not create an ownerless merchant")
}

func TestDelete_RequiresExport(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t)
	tn, err := svc.Provision(ctx, ProvisionRequest{Slug: "acme", PermissionGroupID: "group-acme"})
	require.NoError(t, err)

	// Seed a merchant-owned row + a secret so the purge has something to remove.
	_, err = svc.pool.Exec(ctx, `
		WITH subject AS (
			INSERT INTO openrails.customers (merchant_id)
			VALUES ($1::uuid)
			RETURNING id
		)
		INSERT INTO openrails.entitlements (merchant_id, customer_id)
		SELECT $1::uuid, id FROM subject
	`, tn.ID.String())
	require.NoError(t, err)
	_, err = svc.secrets.Put(ctx, tn.ID, SecretStripeSecretKey, "sk")
	require.NoError(t, err)

	// Delete without confirm -> error.
	require.Error(t, svc.Delete(ctx, tn.ID, DeleteOptions{Confirm: false}))

	// Delete with confirm but NO export -> ErrExportRequired.
	err = svc.Delete(ctx, tn.ID, DeleteOptions{Confirm: true})
	require.True(t, errors.Is(err, ErrExportRequired), "delete must require export, got %v", err)

	// Export, then delete succeeds and purges rows.
	exportID, counts, err := svc.Export(ctx, tn.ID)
	require.NoError(t, err)
	require.NotEmpty(t, exportID)
	require.Equal(t, 1, counts["entitlements"])

	require.NoError(t, svc.Delete(ctx, tn.ID, DeleteOptions{Confirm: true}))

	var entCount int
	require.NoError(t, svc.pool.QueryRow(ctx, `SELECT count(*) FROM openrails.entitlements WHERE merchant_id=$1::uuid`, tn.ID.String()).Scan(&entCount))
	require.Equal(t, 0, entCount, "delete must purge merchant-owned rows")

	// The directory row is tombstoned (no longer resolvable as active).
	_, err = svc.Get(ctx, tn.ID)
	require.True(t, errors.Is(err, ErrMerchantNotFound))
}

func TestCredentialRotation_LoadsProviderAccountScopedSecret(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t)
	tn, err := svc.Provision(ctx, ProvisionRequest{Slug: "acme", PermissionGroupID: "group-acme"})
	require.NoError(t, err)

	seedProviderAccount(t, svc, tn.ID, "stripe", "live", "acct_test")
	secretName, err := ProviderAccountSecretName("stripe", "live", "acct_test", "secret_key")
	require.NoError(t, err)
	sec, err := svc.secrets.Put(ctx, tn.ID, secretName, "sk_1")
	require.NoError(t, err)
	require.Equal(t, 1, sec.Version)

	sec, err = svc.secrets.Put(ctx, tn.ID, secretName, "sk_2")
	require.NoError(t, err)
	require.Equal(t, 2, sec.Version)

	// Loaded credentials reflect the rotated value.
	creds, err := svc.LoadStripeCredentials(ctx, tn.ID)
	require.NoError(t, err)
	require.Equal(t, "sk_2", creds.SecretKey)

}

func TestWebhookRouting_ResolvesThenCallerVerifies(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t)
	tn, err := svc.Provision(ctx, ProvisionRequest{Slug: "acme", PermissionGroupID: "group-acme"})
	require.NoError(t, err)

	// Resolve by slug.
	route, err := svc.ResolveBySlug(ctx, "acme")
	require.NoError(t, err)
	require.Equal(t, tn.ID, route.MerchantID)

	// Unknown slug is unresolved (caller must reject; never default-fallback).
	_, err = svc.ResolveBySlug(ctx, "nope")
	require.True(t, errors.Is(err, ErrMerchantRouteUnresolved))

	// After resolution the caller loads THAT merchant's signing secret (the trust
	// boundary), which is namespaced to the merchant.
	seedProviderAccount(t, svc, tn.ID, "stripe", "live", "acct_acme")
	secretName, err := ProviderAccountSecretName("stripe", "live", "acct_acme", "webhook_signing_secret")
	require.NoError(t, err)
	_, err = svc.secrets.Put(ctx, tn.ID, secretName, "whsec_acme")
	require.NoError(t, err)
	creds, err := svc.LoadStripeCredentials(ctx, route.MerchantID)
	require.NoError(t, err)
	require.Equal(t, "whsec_acme", creds.WebhookSigningSecret)

	// A deleted merchant no longer resolves.
	exportID, _, err := svc.Export(ctx, tn.ID)
	require.NoError(t, err)
	require.NotEmpty(t, exportID)
	require.NoError(t, svc.Delete(ctx, tn.ID, DeleteOptions{Confirm: true}))
	_, err = svc.ResolveBySlug(ctx, "acme")
	require.ErrorIs(t, err, ErrMerchantRouteUnresolved)
}
