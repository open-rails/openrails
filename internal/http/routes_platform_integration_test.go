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

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/platform"
	"github.com/open-rails/openrails/pkg/authprovider"
)

const platformSchemaDDL = `
CREATE SCHEMA IF NOT EXISTS openrails;
CREATE TABLE IF NOT EXISTS openrails.merchants (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug         TEXT NOT NULL UNIQUE,
    status       TEXT NOT NULL DEFAULT 'active',
    owner_org_id TEXT,
    provisioned_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    suspended_at TIMESTAMPTZ, deleted_at TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS openrails.subscriptions (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), merchant_id UUID, status TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS openrails.payments (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), merchant_id UUID, amount BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'completed');
INSERT INTO openrails.merchants (slug, status) VALUES ('acme','active') ON CONFLICT DO NOTHING;
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
		dbtest.WithPostgresLimits(),
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

type fakeSuperadminChecker struct{ allow map[string]bool }

func (f fakeSuperadminChecker) HasPlatformSuperadmin(_ context.Context, userID string) (bool, error) {
	return f.allow[userID], nil
}

func newPlatformServer(t *testing.T, pool *pgxpool.Pool) *Server {
	t.Helper()
	dbPool := db.WrapPool(pool, "")
	tsvc, err := merchants.NewService(dbPool, merchants.NewMemorySecretStore())
	require.NoError(t, err)
	metrics, err := platform.NewMetrics(dbPool)
	require.NoError(t, err)
	return &Server{merchants: tsvc, platformMetrics: metrics}
}

func doReq(t *testing.T, s *Server, checker ginmw.PlatformSuperadminChecker, method, path string, uc authprovider.UserContext, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(func(c *gin.Context) { c.Set("openrails.user_context", uc); c.Next() })
	g := e.Group(StandaloneV1Prefix + PlatformPrefix)
	g.Use(ginmw.UserSessionPlatformPrincipalRequired(checker))
	g.Use(ginmw.RequirePermission(controlplane.PermPlatformSuperadmin))
	g.GET("/merchants", s.platformListMerchantsHandler())
	g.GET("/search", s.platformSearchHandler())
	g.GET("/metrics", s.platformMetricsHandler())

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

func doMerchantDirectoryReq(t *testing.T, s *Server, checker ginmw.PlatformSuperadminChecker, method, path string, uc authprovider.UserContext, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(func(c *gin.Context) { c.Set("openrails.user_context", uc); c.Next() })
	g := e.Group(StandaloneV1Prefix + MerchantAdminPrefix)
	g.Use(ginmw.UserSessionPlatformPrincipalRequired(checker))
	g.Use(ginmw.RequirePermission(controlplane.PermPlatformSuperadmin))
	g.GET("/:id", s.merchantGetHandler())

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

func TestPlatformAPI_GateSearchAndMetrics(t *testing.T) {
	pool := newPlatformTestPool(t)
	checker := fakeSuperadminChecker{allow: map[string]bool{"platform-admin": true}}
	s := newPlatformServer(t, pool)

	base := StandaloneV1Prefix + PlatformPrefix
	platformUC := authprovider.UserContext{UserID: "platform-admin", Org: "openrails-platform"}
	merchantAdminUC := authprovider.UserContext{UserID: "merchant-admin", Org: "tenant-acme", OrgRoles: []string{"admin"}}

	rr := doReq(t, s, checker, http.MethodGet, base+"/merchants", merchantAdminUC, "")
	require.Equal(t, http.StatusForbidden, rr.Code, "merchant operator admin must be denied")

	rr = doReq(t, s, checker, http.MethodGet, base+"/merchants", platformUC, "")
	require.Equal(t, http.StatusOK, rr.Code, "platform identity must pass: %s", rr.Body.String())

	rr = doReq(t, s, checker, http.MethodGet, base+"/search?q=acme", platformUC, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	rr = doReq(t, s, checker, http.MethodGet, base+"/metrics", platformUC, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var m platform.PlatformMetrics
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &m))
	require.Equal(t, 1, m.MerchantCount)
}

func TestMerchantDirectoryAPI_IsPlatformOnly(t *testing.T) {
	pool := newPlatformTestPool(t)
	checker := fakeSuperadminChecker{allow: map[string]bool{"platform-admin": true}}
	s := newPlatformServer(t, pool)

	var merchantID string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT id::text FROM openrails.merchants WHERE slug = 'acme'`).Scan(&merchantID))

	base := StandaloneV1Prefix + MerchantAdminPrefix + "/" + merchantID
	platformUC := authprovider.UserContext{UserID: "platform-admin", Org: "openrails-platform"}
	merchantAdminUC := authprovider.UserContext{UserID: "merchant-admin", Org: "tenant-acme", OrgRoles: []string{"admin"}}

	rr := doMerchantDirectoryReq(t, s, checker, http.MethodGet, base, merchantAdminUC, "")
	require.Equal(t, http.StatusForbidden, rr.Code, "merchant operator admin must not reach the global merchant directory")

	rr = doMerchantDirectoryReq(t, s, checker, http.MethodGet, base, platformUC, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
}
