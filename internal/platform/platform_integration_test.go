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

	"github.com/open-rails/openrails/pkg/tenant"
)

// schemaDDL stands up the minimal billing.* schema the platform layer touches:
// the tenant directory, the two #226 control-plane tables, and representative
// subscriptions/payments tables (with tenant_id) so metrics can aggregate.
const schemaDDL = `
CREATE SCHEMA IF NOT EXISTS billing;

CREATE TABLE IF NOT EXISTS billing.tenants (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug         TEXT NOT NULL UNIQUE,
    name         TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active',
    billing_tier TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    deleted_at   TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS billing.subscriptions (
    id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID,
    status    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS billing.payments (
    id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID,
    amount    BIGINT NOT NULL,
    status    TEXT NOT NULL DEFAULT 'completed'
);

CREATE TABLE IF NOT EXISTS billing.platform_audit (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_user_id    TEXT NOT NULL,
    actor_org        TEXT,
    action           TEXT NOT NULL,
    target_tenant_id UUID,
    reason           TEXT,
    before_state     JSONB,
    after_state      JSONB,
    detail           JSONB,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

CREATE TABLE IF NOT EXISTS billing.platform_break_glass (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_user_id    TEXT NOT NULL,
    target_tenant_id UUID,
    justification    TEXT NOT NULL,
    granted_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    expires_at       TIMESTAMPTZ NOT NULL,
    revoked_at       TIMESTAMPTZ,
    CONSTRAINT chk_break_glass_window CHECK (expires_at > granted_at),
    CONSTRAINT chk_break_glass_justified CHECK (length(btrim(justification)) > 0)
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

func seedTenant(t *testing.T, pool *pgxpool.Pool, slug, status, tier string) tenant.ID {
	t.Helper()
	var idStr string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO billing.tenants (slug, name, status, billing_tier)
		VALUES ($1, $1, $2, NULLIF($3,'')) RETURNING id::text
	`, slug, status, tier).Scan(&idStr)
	require.NoError(t, err)
	id, err := tenant.ParseID(idStr)
	require.NoError(t, err)
	return id
}

func TestAuditLog_RecordAndList(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	tid := seedTenant(t, pool, "acme", "active", "pro")

	audit, err := NewAuditLog(pool)
	require.NoError(t, err)

	id, err := audit.Record(ctx, AuditEntry{
		ActorUserID:    "platform-admin",
		ActorOrg:       "openrails-platform",
		Action:         ActionTenantSuspend,
		TargetTenantID: &tid,
		Reason:         "abuse report",
		Before:         map[string]any{"status": "active"},
		After:          map[string]any{"status": "suspended"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	// A second, platform-wide action (no target).
	_, err = audit.Record(ctx, AuditEntry{ActorUserID: "platform-admin", Action: ActionMetricsRead})
	require.NoError(t, err)

	// Missing actor / action are rejected (every action is attributable).
	_, err = audit.Record(ctx, AuditEntry{Action: ActionMetricsRead})
	require.Error(t, err)
	_, err = audit.Record(ctx, AuditEntry{ActorUserID: "x"})
	require.Error(t, err)

	// List all.
	all, err := audit.List(ctx, nil, 0)
	require.NoError(t, err)
	require.Len(t, all, 2)

	// List filtered to the target tenant returns only the targeted row, with the
	// before/after captured.
	scoped, err := audit.List(ctx, &tid, 0)
	require.NoError(t, err)
	require.Len(t, scoped, 1)
	require.Equal(t, ActionTenantSuspend, scoped[0].Action)
	require.Equal(t, "abuse report", scoped[0].Reason)

	// before_state / after_state persisted.
	var before, after string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT before_state::text, after_state::text FROM billing.platform_audit WHERE id=$1::uuid`, id).
		Scan(&before, &after))
	require.Contains(t, before, "active")
	require.Contains(t, after, "suspended")
}

func TestBreakGlass_GrantExpiresAuditsAndAlerts(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	tid := seedTenant(t, pool, "acme", "active", "pro")

	audit, err := NewAuditLog(pool)
	require.NoError(t, err)
	bg, err := NewBreakGlass(pool, audit)
	require.NoError(t, err)

	// Justification is required.
	_, err = bg.Grant(ctx, GrantRequest{ActorUserID: "admin", TargetTenant: &tid})
	require.ErrorIs(t, err, ErrBreakGlassJustificationRequired)

	// Over-long TTL is clamped.
	grant, err := bg.Grant(ctx, GrantRequest{
		ActorUserID:   "admin",
		TargetTenant:  &tid,
		Justification: "incident #42: investigate stuck dunning",
		TTL:           100 * time.Hour,
	})
	require.NoError(t, err)
	require.NotEmpty(t, grant.ID)
	require.WithinDuration(t, grant.GrantedAt.Add(MaxBreakGlassTTL), grant.ExpiresAt, time.Minute,
		"break-glass TTL must be clamped to MaxBreakGlassTTL")
	require.True(t, grant.Active(time.Now()))

	// The grant is audited (a platform_audit row with the break-glass action).
	rows, err := audit.List(ctx, &tid, 0)
	require.NoError(t, err)
	var found bool
	for _, r := range rows {
		if r.Action == ActionBreakGlassGrant {
			found = true
			require.Contains(t, r.Reason, "incident #42")
		}
	}
	require.True(t, found, "break-glass grant must be audited")

	// It is currently active for the actor.
	active, err := bg.IsActive(ctx, "admin", &tid)
	require.NoError(t, err)
	require.True(t, active)

	// A DIFFERENT actor has no active grant.
	active, err = bg.IsActive(ctx, "someone-else", &tid)
	require.NoError(t, err)
	require.False(t, active)

	// Expiry: a grant whose expiry is in the past is not active. Force the whole
	// window into the past (granted_at too, to keep expires_at > granted_at).
	_, err = pool.Exec(ctx, `
		UPDATE billing.platform_break_glass
		   SET granted_at = current_timestamp - interval '2 hours',
		       expires_at = current_timestamp - interval '1 minute'
		 WHERE id=$1::uuid`, grant.ID)
	require.NoError(t, err)
	active, err = bg.IsActive(ctx, "admin", &tid)
	require.NoError(t, err)
	require.False(t, active, "expired break-glass must not be active")

	listed, err := bg.ListActive(ctx)
	require.NoError(t, err)
	require.Empty(t, listed, "expired grant must not be listed active")

	// Revoke audits a revocation and is idempotent.
	live, err := bg.Grant(ctx, GrantRequest{ActorUserID: "admin", TargetTenant: &tid, Justification: "second incident"})
	require.NoError(t, err)
	require.NoError(t, bg.Revoke(ctx, live.ID, "admin", "openrails-platform"))
	require.NoError(t, bg.Revoke(ctx, live.ID, "admin", "openrails-platform")) // idempotent

	active, err = bg.IsActive(ctx, "admin", &tid)
	require.NoError(t, err)
	require.False(t, active, "revoked break-glass must not be active")

	// A revoke action is recorded.
	rows, err = audit.List(ctx, nil, 0)
	require.NoError(t, err)
	var revokeFound bool
	for _, r := range rows {
		if r.Action == ActionBreakGlassRevoke {
			revokeFound = true
		}
	}
	require.True(t, revokeFound, "break-glass revoke must be audited")

	// Unknown grant id -> not found.
	require.ErrorIs(t, bg.Revoke(ctx, "00000000-0000-0000-0000-000000000099", "admin", ""), ErrBreakGlassNotFound)
}

func TestMetrics_AggregatesAcrossTenants(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	acme := seedTenant(t, pool, "acme", "active", "pro")
	globex := seedTenant(t, pool, "globex", "suspended", "free")
	_ = seedTenant(t, pool, "empty", "active", "")

	// acme: 2 active subs, 1 failed sub, 2 completed payments (500 + 1500), 1 refunded (ignored).
	exec := func(q string, args ...any) {
		_, err := pool.Exec(ctx, q, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO billing.subscriptions (tenant_id, status) VALUES ($1::uuid,'active'),($1::uuid,'active'),($1::uuid,'failed')`, acme.String())
	exec(`INSERT INTO billing.payments (tenant_id, amount, status) VALUES ($1::uuid,500,'completed'),($1::uuid,1500,'completed'),($1::uuid,999,'refunded')`, acme.String())
	// globex: 1 active sub, 1 completed payment (300).
	exec(`INSERT INTO billing.subscriptions (tenant_id, status) VALUES ($1::uuid,'active')`, globex.String())
	exec(`INSERT INTO billing.payments (tenant_id, amount, status) VALUES ($1::uuid,300,'completed')`, globex.String())

	metrics, err := NewMetrics(pool)
	require.NoError(t, err)
	m, err := metrics.Compute(ctx)
	require.NoError(t, err)

	require.Equal(t, 3, m.TenantCount)
	require.Equal(t, 2, m.ActiveTenantCount)
	require.Equal(t, 1, m.SuspendedTenantCount)
	require.Equal(t, int64(3), m.TotalActiveSubs)              // 2 + 1
	require.Equal(t, int64(500+1500+300), m.TotalRevenueMinor) // refunded excluded
	require.Equal(t, int64(1), m.TotalWebhookFailures)         // 1 failed sub

	byTenant := map[string]TenantMetric{}
	for _, tm := range m.Tenants {
		byTenant[tm.Slug] = tm
	}
	require.Equal(t, int64(2), byTenant["acme"].ActiveSubs)
	require.Equal(t, int64(2000), byTenant["acme"].RevenueMinor)
	require.Equal(t, int64(1), byTenant["acme"].WebhookFailures)
	require.Equal(t, int64(0), byTenant["empty"].ActiveSubs)
	require.Equal(t, int64(0), byTenant["empty"].RevenueMinor)
}
