//go:build integration

package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestMerchantWebhookRouteHTTPResolvesMerchantBeforeVerifyingStripe(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantWebhookRoutePool(t)
	secrets := merchants.NewMemorySecretStore()
	svc, err := merchants.NewService(db.WrapPool(pool, ""), secrets)
	require.NoError(t, err)

	acme, err := svc.Provision(ctx, merchants.ProvisionRequest{Slug: "acme", PermissionGroupID: "group-acme"})
	require.NoError(t, err)
	evil, err := svc.Provision(ctx, merchants.ProvisionRequest{Slug: "evil", PermissionGroupID: "group-evil"})
	require.NoError(t, err)
	seedProviderAccount(t, pool, acme.ID.String(), "stripe", "acct_acme")
	seedProviderAccount(t, pool, evil.ID.String(), "stripe", "acct_evil")
	seedProviderAccount(t, pool, acme.ID.String(), "nmi", "nmi_acme_account")
	seedProviderAccount(t, pool, evil.ID.String(), "nmi", "nmi_evil_account")
	putProviderSecret(t, ctx, secrets, acme.ID, "stripe", "acct_acme", "webhook_signing_secret", "whsec_acme")
	putProviderSecret(t, ctx, secrets, evil.ID, "stripe", "acct_evil", "webhook_signing_secret", "whsec_evil")
	putProviderSecret(t, ctx, secrets, acme.ID, "nmi", "nmi_acme_account", "webhook_signing_secret", "nmi_acme")
	putProviderSecret(t, ctx, secrets, evil.ID, "nmi", "nmi_evil_account", "webhook_signing_secret", "nmi_evil")

	rt := &app.Runtime{Merchants: svc}
	mux := http.NewServeMux()
	httproutes.RegisterMerchantWebhookRoutes(router.NewMux(mux, "/v1", rt), rt)
	hostGroup := router.NewMux(mux, "/host-webhooks", rt).Group("", func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			r.Request = r.Request.WithContext(merchant.WithID(r.Request.Context(), acme.ID))
			next(r)
		}
	})
	httproutes.RegisterHostWebhookRoutes(hostGroup, rt)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	body := []byte(`{"id":"evt_1","type":"checkout.session.completed"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	require.Equal(t, http.StatusNotFound, postMerchantWebhook(t, server.URL+"/v1/merchants/nope/webhooks/stripe", body, stripeSig("whsec_acme", ts, body)))
	require.Equal(t, http.StatusUnauthorized, postMerchantWebhook(t, server.URL+"/v1/merchants/acme/webhooks/stripe", body, stripeSig("whsec_evil", ts, body)))
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhook(t, server.URL+"/v1/merchants/acme/webhooks/stripe", body, stripeSig("whsec_acme", ts, body)))
	require.Equal(t, http.StatusUnauthorized, postMerchantWebhook(t, server.URL+"/host-webhooks/stripe", body, stripeSig("whsec_evil", ts, body)))
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhook(t, server.URL+"/host-webhooks/stripe", body, stripeSig("whsec_acme", ts, body)))

	nmiBody := []byte(`{"event_id":"evt_nmi_1","event_type":"transaction.sale.success","event_body":{}}`)
	require.Equal(t, http.StatusUnauthorized, postMerchantWebhookWithHeader(t, server.URL+"/v1/merchants/acme/webhooks/mobius", nmiBody, "Webhook-Signature", nmiSig("nmi_evil", ts, nmiBody)))
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhookWithHeader(t, server.URL+"/v1/merchants/acme/webhooks/mobius", nmiBody, "Webhook-Signature", nmiSig("nmi_acme", ts, nmiBody)))

	ccbillBody := []byte(`{"eventType":"RenewalSuccess"}`)
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhookWithHeader(t, server.URL+"/v1/merchants/acme/webhooks/ccbill?eventType=RenewalSuccess", ccbillBody, "X-Unused", "unused"))
}

func postMerchantWebhook(t *testing.T, url string, body []byte, sig string) int {
	return postMerchantWebhookWithHeader(t, url, body, "Stripe-Signature", sig)
}

func postMerchantWebhookWithHeader(t *testing.T, url string, body []byte, header string, sig string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set(header, sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

func seedProviderAccount(t *testing.T, pool *pgxpool.Pool, merchantID, provider, accountID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO openrails.provider_accounts (merchant_id, provider_type, environment, account_id, role, status)
		VALUES ($1::uuid, $2, 'live', $3, 'primary', 'enabled')
	`, merchantID, provider, accountID)
	require.NoError(t, err)
}

func putProviderSecret(t *testing.T, ctx context.Context, store merchants.MerchantSecretStore, merchantID merchant.ID, provider, accountID, key, value string) {
	t.Helper()
	name, err := merchants.ProviderAccountSecretName(provider, "live", accountID, key)
	require.NoError(t, err)
	_, err = store.Put(ctx, merchantID, name, value)
	require.NoError(t, err)
}

func nmiSig(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return fmt.Sprintf("t=%s,s=%s", ts, hex.EncodeToString(mac.Sum(nil)))
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
		dbtest.WithPostgresLimits(),
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
			permission_group_id text,
			created_at timestamptz NOT NULL DEFAULT current_timestamp,
			updated_at timestamptz NOT NULL DEFAULT current_timestamp,
			deleted_at timestamptz
		);
		CREATE TABLE IF NOT EXISTS openrails.provider_accounts (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			merchant_id uuid NOT NULL REFERENCES openrails.merchants(id) ON DELETE CASCADE,
			provider_type text NOT NULL,
			environment text NOT NULL DEFAULT 'live',
			account_id text NOT NULL,
			display_name text,
			vault_secret_ref text,
			role text NOT NULL DEFAULT 'primary',
			status text NOT NULL DEFAULT 'enabled',
			evidence jsonb,
			first_seen_at timestamptz NOT NULL DEFAULT current_timestamp,
			last_verified_at timestamptz,
			replaced_at timestamptz,
			created_at timestamptz NOT NULL DEFAULT current_timestamp,
			updated_at timestamptz NOT NULL DEFAULT current_timestamp
		);
	`)
	require.NoError(t, err)
}
