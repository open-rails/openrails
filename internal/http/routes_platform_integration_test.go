//go:build integration

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/platform"
	"github.com/open-rails/openrails/internal/tenancy"
	"github.com/open-rails/openrails/pkg/authprovider"
)

const platformSchemaDDL = `
CREATE SCHEMA IF NOT EXISTS billing;
CREATE TABLE IF NOT EXISTS billing.tenants (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug         TEXT NOT NULL UNIQUE,
    name         TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active',
    authkit_org_id TEXT, authkit_org_slug TEXT,
    billing_tier TEXT, region TEXT, webhook_host TEXT, webhook_path TEXT,
    provisioned_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    suspended_at TIMESTAMPTZ, deleted_at TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS billing.subscriptions (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), tenant_id UUID, status TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS billing.payments (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), tenant_id UUID, amount BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'completed');
CREATE TABLE IF NOT EXISTS billing.platform_audit (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(), actor_user_id TEXT NOT NULL, actor_org TEXT,
    action TEXT NOT NULL, target_tenant_id UUID, reason TEXT,
    before_state JSONB, after_state JSONB, detail JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);
CREATE TABLE IF NOT EXISTS billing.platform_break_glass (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(), actor_user_id TEXT NOT NULL, target_tenant_id UUID,
    justification TEXT NOT NULL, granted_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    expires_at TIMESTAMPTZ NOT NULL, revoked_at TIMESTAMPTZ,
    CONSTRAINT chk_break_glass_window CHECK (expires_at > granted_at)
);
INSERT INTO billing.tenants (slug, name, status, billing_tier) VALUES ('acme','Acme','active','pro') ON CONFLICT DO NOTHING;
`

func newPlatformTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	if dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN")); dsn != "" {
		pool := newPlatformExternalPostgresPool(t, ctx, dsn)
		_, err := pool.Exec(ctx, platformSchemaDDL)
		require.NoError(t, err)
		return pool
	}

	c, err := postgres.Run(ctx, "postgres:18-alpine",
		postgres.WithDatabase("openrails"), postgres.WithUsername("test"), postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, platformSchemaDDL)
	require.NoError(t, err)
	return pool
}

func newPlatformExternalPostgresPool(t *testing.T, ctx context.Context, adminDSN string) *pgxpool.Pool {
	t.Helper()
	adminCfg, err := pgxpool.ParseConfig(adminDSN)
	require.NoError(t, err)
	adminCfg.ConnConfig.Config.Database = "postgres"
	adminPool, err := pgxpool.NewWithConfig(ctx, adminCfg)
	require.NoError(t, err)

	dbName := fmt.Sprintf("openrails_platform_%d", time.Now().UnixNano())
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

// fakeSuperadminChecker stands in for the control-plane platform-superadmin
// check so the HTTP layer can be exercised without a live AuthKit.
type fakeSuperadminChecker struct{ allow map[string]bool }

func (f fakeSuperadminChecker) HasPlatformSuperadmin(_ context.Context, userID string) (bool, error) {
	return f.allow[userID], nil
}

// newPlatformServer builds a minimal *Server with just the platform deps wired.
func newPlatformServer(t *testing.T, pool *pgxpool.Pool) *Server {
	t.Helper()
	tsvc, err := tenancy.NewService(pool, nil, tenancy.NewMemorySecretStore())
	require.NoError(t, err)
	audit, err := platform.NewAuditLog(pool)
	require.NoError(t, err)
	bg, err := platform.NewBreakGlass(pool, audit)
	require.NoError(t, err)
	metrics, err := platform.NewMetrics(pool)
	require.NoError(t, err)
	return &Server{tenancy: tsvc, platformAudit: audit, platformBreakGlass: bg, platformMetrics: metrics}
}

// doReq builds a fresh engine seeding uc (as the auth middleware would) behind
// the REAL superadmin gate, then serves one request.
func doReq(t *testing.T, s *Server, checker authpolicy.PlatformSuperadminChecker, method, path string, uc authprovider.UserContext, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(func(c *gin.Context) { c.Set("billing.user_context", uc); c.Next() })
	g := e.Group(StandaloneV1Prefix + PlatformPrefix)
	g.Use(authpolicy.PlatformSuperadminRequired(checker))
	g.GET("/tenants", s.platformListTenantsHandler())
	g.GET("/search", s.platformSearchHandler())
	g.GET("/metrics", s.platformMetricsHandler())
	g.GET("/audit", s.platformAuditHandler())
	g.POST("/break-glass", s.platformBreakGlassGrantHandler())

	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rr := httptest.NewRecorder()
	e.ServeHTTP(rr, req)
	return rr
}

func TestPlatformAPI_GateAndAudit(t *testing.T) {
	pool := newPlatformTestPool(t)
	checker := fakeSuperadminChecker{allow: map[string]bool{"platform-admin": true}}
	s := newPlatformServer(t, pool)

	base := StandaloneV1Prefix + PlatformPrefix

	platformUC := authprovider.UserContext{UserID: "platform-admin", Org: "openrails-platform"}
	tenantAdminUC := authprovider.UserContext{UserID: "tenant-admin", Org: "tenant-acme", OrgRoles: []string{"admin"}}

	// 1. A tenant operator admin is DENIED the platform surface.
	rr := doReq(t, s, checker, http.MethodGet, base+"/tenants", tenantAdminUC, "")
	require.Equal(t, http.StatusForbidden, rr.Code, "tenant operator admin must be denied")

	// 2. A platform identity is ALLOWED.
	rr = doReq(t, s, checker, http.MethodGet, base+"/tenants", platformUC, "")
	require.Equal(t, http.StatusOK, rr.Code, "platform identity must pass: %s", rr.Body.String())

	// 3. Cross-tenant search writes an audit row.
	rr = doReq(t, s, checker, http.MethodGet, base+"/search?q=acme", platformUC, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var searchCount int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM billing.platform_audit WHERE action = $1 AND reason = 'acme'`,
		platform.ActionTenantSearch).Scan(&searchCount))
	require.Equal(t, 1, searchCount, "cross-tenant search must write an audit row")

	// 4. Metrics aggregates and is audited.
	rr = doReq(t, s, checker, http.MethodGet, base+"/metrics", platformUC, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var m platform.PlatformMetrics
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &m))
	require.Equal(t, 1, m.TenantCount)

	// 5. Break-glass requires a justification.
	rr = doReq(t, s, checker, http.MethodPost, base+"/break-glass", platformUC, `{"justification":""}`)
	require.Equal(t, http.StatusBadRequest, rr.Code)

	rr = doReq(t, s, checker, http.MethodPost, base+"/break-glass", platformUC, `{"justification":"incident 7","ttl_seconds":600}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var grantCount int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM billing.platform_break_glass WHERE actor_user_id='platform-admin'`).Scan(&grantCount))
	require.Equal(t, 1, grantCount, "break-glass grant must be persisted")
}
