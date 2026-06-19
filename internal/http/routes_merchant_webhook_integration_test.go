//go:build integration

package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/merchants"
)

func TestMerchantWebhookRouteHTTPResolvesMerchantBeforeVerifyingStripe(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantWebhookRoutePool(t)
	secrets := merchants.NewMemorySecretStore()
	svc, err := merchants.NewService(db.WrapPool(pool, ""), secrets)
	require.NoError(t, err)

	acme, err := svc.Provision(ctx, merchants.ProvisionRequest{Slug: "acme", OwnerOrgID: "org-acme"})
	require.NoError(t, err)
	evil, err := svc.Provision(ctx, merchants.ProvisionRequest{Slug: "evil", OwnerOrgID: "org-evil"})
	require.NoError(t, err)
	_, err = secrets.Put(ctx, acme.ID, merchants.SecretStripeWebhookSigning, "whsec_acme")
	require.NoError(t, err)
	_, err = secrets.Put(ctx, evil.ID, merchants.SecretStripeWebhookSigning, "whsec_evil")
	require.NoError(t, err)

	rt := &app.Runtime{Merchants: svc}
	mux := http.NewServeMux()
	httproutes.RegisterMerchantWebhookRoutes(router.NewMux(mux, "/v1", rt), rt)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	body := []byte(`{"id":"evt_1","type":"checkout.session.completed"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	require.Equal(t, http.StatusNotFound, postMerchantWebhook(t, server.URL+"/v1/merchants/nope/webhooks/stripe", body, stripeSig("whsec_acme", ts, body)))
	require.Equal(t, http.StatusUnauthorized, postMerchantWebhook(t, server.URL+"/v1/merchants/acme/webhooks/stripe", body, stripeSig("whsec_evil", ts, body)))
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhook(t, server.URL+"/v1/merchants/acme/webhooks/stripe", body, stripeSig("whsec_acme", ts, body)))
}

func postMerchantWebhook(t *testing.T, url string, body []byte, sig string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Stripe-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

func newMerchantWebhookRoutePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	if dsn := strings.TrimSpace(testDBDSN()); dsn != "" {
		pool := newMerchantWebhookExternalPool(t, ctx, dsn)
		applyMerchantWebhookRouteSchema(t, ctx, pool)
		return pool
	}
	container, err := postgres.Run(ctx,
		"postgres:18-alpine",
		postgres.WithDatabase("openrails"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	applyMerchantWebhookRouteSchema(t, ctx, pool)
	return pool
}

func testDBDSN() string {
	if dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_URL")); dsn != "" {
		return dsn
	}
	return strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN"))
}

func newMerchantWebhookExternalPool(t *testing.T, ctx context.Context, adminDSN string) *pgxpool.Pool {
	t.Helper()
	adminCfg, err := pgxpool.ParseConfig(adminDSN)
	require.NoError(t, err)
	adminCfg.ConnConfig.Config.Database = "postgres"
	adminPool, err := pgxpool.NewWithConfig(ctx, adminCfg)
	require.NoError(t, err)

	dbName := "openrails_webhook_route_" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
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

func applyMerchantWebhookRouteSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		CREATE EXTENSION IF NOT EXISTS pgcrypto;
		CREATE SCHEMA IF NOT EXISTS openrails;
		CREATE TABLE IF NOT EXISTS openrails.merchants (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			slug text NOT NULL UNIQUE,
			status text NOT NULL DEFAULT 'active',
			owner_org_id text,
			provisioned_at timestamptz,
			created_at timestamptz NOT NULL DEFAULT current_timestamp,
			updated_at timestamptz NOT NULL DEFAULT current_timestamp,
			suspended_at timestamptz,
			deleted_at timestamptz
		);
	`)
	require.NoError(t, err)
}
