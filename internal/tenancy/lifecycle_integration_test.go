//go:build integration

package tenancy

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

	"github.com/open-rails/openrails/pkg/tenant"
)

// schemaDDL stands up the minimal billing.* schema the tenancy service touches:
// the tenant directory (with #225 columns), one representative tenant-owned table
// (entitlements) so export/delete have rows to purge, and the #225 control-plane
// tables. It mirrors migrations 039 + 041 for the columns under test.
const schemaDDL = `
CREATE SCHEMA IF NOT EXISTS billing;

CREATE TABLE IF NOT EXISTS billing.tenants (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug             TEXT NOT NULL UNIQUE,
    name             TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'active',
    authkit_tenant_id   TEXT,
    authkit_tenant_slug TEXT,
    plan             TEXT,
    region           TEXT,
    billing_tier     TEXT,
    stripe_account_id TEXT,
    webhook_host     TEXT,
    webhook_path     TEXT,
    provisioned_at   TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    suspended_at     TIMESTAMPTZ,
    deleted_at       TIMESTAMPTZ
);
INSERT INTO billing.tenants (id, slug, name)
VALUES ('00000000-0000-0000-0000-000000000001', 'default', 'Default')
ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS billing.entitlements (
    id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID,
    user_id   TEXT
);

CREATE TABLE IF NOT EXISTS billing.tenant_secrets (
    tenant_id  UUID NOT NULL,
    name       TEXT NOT NULL,
    value      TEXT NOT NULL,
    version    INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    PRIMARY KEY (tenant_id, name)
);

CREATE TABLE IF NOT EXISTS billing.tenant_credential_audit (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL,
    name       TEXT NOT NULL,
    action     TEXT NOT NULL,
    actor      TEXT,
    detail     TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

CREATE TABLE IF NOT EXISTS billing.tenant_exports (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL,
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

// fakeTenantProvisioner records EnsureAuthKitTenant calls and returns a stable id.
type fakeTenantProvisioner struct{ calls int }

func (f *fakeTenantProvisioner) EnsureAuthKitTenant(_ context.Context, slug string) (string, error) {
	f.calls++
	return "org-" + slug, nil
}

func newSvc(t *testing.T) (*Service, *fakeTenantProvisioner) {
	pool := newTestPool(t)
	orgs := &fakeTenantProvisioner{}
	svc, err := NewService(pool, orgs, NewMemorySecretStore())
	require.NoError(t, err)
	return svc, orgs
}

func TestProvision_Idempotent(t *testing.T) {
	ctx := context.Background()
	svc, orgs := newSvc(t)

	req := ProvisionRequest{Slug: "acme", Name: "Acme", WebhookHost: "acme.example.com", BillingTier: "pro"}
	first, err := svc.Provision(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "acme", first.Slug)
	require.Equal(t, "org-tenant-acme", first.AuthKitTenantID)
	require.Equal(t, "pro", first.BillingTier)

	// Re-provision: same tenant id, no duplicate row, org ensured again (the org
	// provisioner is itself idempotent).
	second, err := svc.Provision(ctx, req)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)

	var count int
	require.NoError(t, svc.pool.QueryRow(ctx, `SELECT count(*) FROM billing.tenants WHERE slug='acme'`).Scan(&count))
	require.Equal(t, 1, count, "provision must not create a duplicate tenant row")
	require.GreaterOrEqual(t, orgs.calls, 2, "org provisioner called on each provision")
}

func TestSuspend_DeniesWritesAllowsReads(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSvc(t)
	tn, err := svc.Provision(ctx, ProvisionRequest{Slug: "acme", Name: "Acme"})
	require.NoError(t, err)

	// Active tenant is writable.
	w, err := svc.IsWritable(ctx, tn.ID)
	require.NoError(t, err)
	require.True(t, w)

	// Suspend -> writes denied, but the row still resolves (safe reads allowed).
	require.NoError(t, svc.Suspend(ctx, tn.ID))
	w, err = svc.IsWritable(ctx, tn.ID)
	require.NoError(t, err)
	require.False(t, w, "suspended tenant must deny writes")

	got, err := svc.Get(ctx, tn.ID)
	require.NoError(t, err, "suspended tenant must still be readable")
	require.Equal(t, StatusSuspended, got.Status)
	require.NotNil(t, got.SuspendedAt)

	// Suspend is idempotent.
	require.NoError(t, svc.Suspend(ctx, tn.ID))

	// Resume restores writes.
	require.NoError(t, svc.Resume(ctx, tn.ID))
	w, err = svc.IsWritable(ctx, tn.ID)
	require.NoError(t, err)
	require.True(t, w)
}

func TestDelete_RequiresExport(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSvc(t)
	tn, err := svc.Provision(ctx, ProvisionRequest{Slug: "acme", Name: "Acme"})
	require.NoError(t, err)

	// Seed a tenant-owned row + a secret so the purge has something to remove.
	_, err = svc.pool.Exec(ctx, `INSERT INTO billing.entitlements (tenant_id, user_id) VALUES ($1::uuid,'u1')`, tn.ID.String())
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
	require.NoError(t, svc.pool.QueryRow(ctx, `SELECT count(*) FROM billing.entitlements WHERE tenant_id=$1::uuid`, tn.ID.String()).Scan(&entCount))
	require.Equal(t, 0, entCount, "delete must purge tenant-owned rows")

	// The directory row is tombstoned (no longer resolvable as active).
	_, err = svc.Get(ctx, tn.ID)
	require.True(t, errors.Is(err, ErrTenantNotFound))

	// Default tenant can never be deleted.
	_, _, err = svc.Export(ctx, tenant.DefaultID)
	require.NoError(t, err)
	require.Error(t, svc.Delete(ctx, tenant.DefaultID, DeleteOptions{Confirm: true}))
}

func TestCredentialRotation_WritesAudit(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSvc(t)
	tn, err := svc.Provision(ctx, ProvisionRequest{Slug: "acme", Name: "Acme"})
	require.NoError(t, err)

	sec, err := svc.PutCredential(ctx, tn.ID, SecretStripeSecretKey, "sk_1", "put", "operator")
	require.NoError(t, err)
	require.Equal(t, 1, sec.Version)

	sec, err = svc.RotateCredential(ctx, tn.ID, SecretStripeSecretKey, "sk_2", "operator")
	require.NoError(t, err)
	require.Equal(t, 2, sec.Version)

	// Loaded credentials reflect the rotated value.
	creds, err := svc.LoadStripeCredentials(ctx, tn.ID)
	require.NoError(t, err)
	require.Equal(t, "sk_2", creds.SecretKey)

	// Audit rows recorded for both put and rotate.
	var auditCount int
	require.NoError(t, svc.pool.QueryRow(ctx,
		`SELECT count(*) FROM billing.tenant_credential_audit WHERE tenant_id=$1::uuid`,
		tn.ID.String()).Scan(&auditCount))
	require.GreaterOrEqual(t, auditCount, 2)

	// TestStripeCredential uses an injected tester (no live Stripe) and audits.
	called := false
	err = svc.TestStripeCredential(ctx, tn.ID, func(_ context.Context, key string) error {
		called = true
		require.Equal(t, "sk_2", key)
		return nil
	})
	require.NoError(t, err)
	require.True(t, called)
}

func TestWebhookRouting_ResolvesThenCallerVerifies(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSvc(t)
	tn, err := svc.Provision(ctx, ProvisionRequest{Slug: "acme", Name: "Acme", WebhookHost: "hooks.acme.com"})
	require.NoError(t, err)

	// Resolve by slug.
	route, err := svc.ResolveBySlug(ctx, "acme")
	require.NoError(t, err)
	require.Equal(t, tn.ID, route.TenantID)

	// Resolve by host (with a port, which must be stripped).
	route, err = svc.ResolveByHost(ctx, "hooks.acme.com:443")
	require.NoError(t, err)
	require.Equal(t, tn.ID, route.TenantID)

	// Unknown slug/host is unresolved (caller must reject; never default-fallback).
	_, err = svc.ResolveBySlug(ctx, "nope")
	require.True(t, errors.Is(err, ErrTenantRouteUnresolved))
	_, err = svc.ResolveByHost(ctx, "nope.example.com")
	require.True(t, errors.Is(err, ErrTenantRouteUnresolved))

	// After resolution the caller loads THAT tenant's signing secret (the trust
	// boundary), which is namespaced to the tenant.
	_, err = svc.secrets.Put(ctx, tn.ID, SecretStripeWebhookSigning, "whsec_acme")
	require.NoError(t, err)
	creds, err := svc.LoadStripeCredentials(ctx, route.TenantID)
	require.NoError(t, err)
	require.Equal(t, "whsec_acme", creds.WebhookSigningSecret)

	// A suspended tenant still RESOLVES (webhooks keep being verified/processed).
	require.NoError(t, svc.Suspend(ctx, tn.ID))
	_, err = svc.ResolveBySlug(ctx, "acme")
	require.NoError(t, err, "suspended tenant must still route webhooks")
}
