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

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestMerchantWebhookRouteHTTPResolvesMerchantBeforeVerifyingStripe(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantWebhookRoutePool(t)
	secrets := merchants.NewMemorySecretStore()
	// Deployment posture is test_mode (#681): the merchants service resolves
	// environment=test rows; live rows are seeded alongside to prove isolation.
	svc, err := merchants.NewService(db.WrapPool(pool, ""), secrets, "test")
	require.NoError(t, err)

	acme, _, err := svc.Provision(ctx, merchants.ProvisionRequest{Slug: "acme", PermissionGroupID: "group-acme"})
	require.NoError(t, err)
	evil, _, err := svc.Provision(ctx, merchants.ProvisionRequest{Slug: "evil", PermissionGroupID: "group-evil"})
	require.NoError(t, err)
	seedRailMerchantAccount(t, pool, acme.ID.String(), "stripe", "acct_acme")
	seedRailMerchantAccount(t, pool, evil.ID.String(), "stripe", "acct_evil")
	seedRailMerchantAccount(t, pool, acme.ID.String(), "nmi", "nmi_acme_account")
	seedRailMerchantAccount(t, pool, evil.ID.String(), "nmi", "nmi_evil_account")
	seedArchivedRailMerchantAccountEnv(t, pool, acme.ID.String(), "nmi", "test", "nmi_acme_archived")
	// NO live ccbill rows anywhere: the #668 test_mode IP-allowlist bypass is
	// refused while any environment=live ccbill account exists in the catalog.
	seedRailMerchantAccountEnv(t, pool, acme.ID.String(), "stripe", "test", "acct_acme_test")
	seedRailMerchantAccountEnv(t, pool, acme.ID.String(), "nmi", "test", "nmi_acme_test")
	seedRailMerchantAccountEnv(t, pool, acme.ID.String(), "ccbill", "test", "945282-0000")
	putProviderSecret(t, ctx, secrets, acme.ID, "stripe", "acct_acme", "webhook_signing_secret", "whsec_acme")
	putProviderSecret(t, ctx, secrets, evil.ID, "stripe", "acct_evil", "webhook_signing_secret", "whsec_evil")
	putProviderSecret(t, ctx, secrets, acme.ID, "nmi", "nmi_acme_account", "webhook_signing_secret", "nmi_acme")
	putProviderSecret(t, ctx, secrets, evil.ID, "nmi", "nmi_evil_account", "webhook_signing_secret", "nmi_evil")
	putProviderSecretEnv(t, ctx, secrets, acme.ID, "nmi", "test", "nmi_acme_archived", "webhook_signing_secret", "nmi_archived")
	putProviderSecretEnv(t, ctx, secrets, acme.ID, "stripe", "test", "acct_acme_test", "webhook_signing_secret", "whsec_acme_test")
	putProviderSecretEnv(t, ctx, secrets, acme.ID, "nmi", "test", "nmi_acme_test", "webhook_signing_secret", "nmi_acme_test")

	rt := &app.Runtime{Config: &config.Config{TestMode: config.CredentialPostureSandbox}, Merchants: svc}
	globalRT := &app.Runtime{Config: &config.Config{TestMode: config.CredentialPostureSandbox}, Merchants: svc}
	mux := http.NewServeMux()
	httproutes.RegisterWebhookRoutes(router.NewMux(mux, "/global", globalRT), globalRT)
	httproutes.RegisterMerchantWebhookRoutes(router.NewMux(mux, "/v1", rt), rt)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	body := []byte(`{"id":"evt_1","type":"checkout.session.completed"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	require.Equal(t, http.StatusNotFound, postMerchantWebhook(t, server.URL+"/v1/merchants/nope/webhooks/stripe", body, stripeSig("whsec_acme_test", ts, body)))
	require.Equal(t, http.StatusUnauthorized, postMerchantWebhook(t, server.URL+"/v1/merchants/acme/webhooks/stripe", body, stripeSig("whsec_evil", ts, body)))
	// test_mode posture resolves the environment=test account's secret (#681);
	// the live account's secret no longer verifies.
	require.Equal(t, http.StatusUnauthorized, postMerchantWebhook(t, server.URL+"/v1/merchants/acme/webhooks/stripe", body, stripeSig("whsec_acme", ts, body)))
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhook(t, server.URL+"/v1/merchants/acme/webhooks/stripe", body, stripeSig("whsec_acme_test", ts, body)))

	nmiBody := []byte(`{"event_id":"evt_nmi_1","event_type":"transaction.sale.success","event_body":{"merchant":{"id":"nmi_acme_test"},"transaction_id":"txn_1"}}`)
	require.Equal(t, http.StatusUnauthorized, postMerchantWebhookWithHeader(t, server.URL+"/v1/merchants/acme/webhooks/mobius", nmiBody, "Webhook-Signature", nmiSig("nmi_evil", ts, nmiBody)))
	require.Equal(t, http.StatusUnauthorized, postMerchantWebhookWithHeader(t, server.URL+"/v1/merchants/acme/webhooks/mobius", nmiBody, "Webhook-Signature", nmiSig("nmi_acme", ts, nmiBody)))
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhookWithHeader(t, server.URL+"/v1/merchants/acme/webhooks/mobius", nmiBody, "Webhook-Signature", nmiSig("nmi_acme_test", ts, nmiBody)))

	ccbillBody := []byte(`{"eventType":"RenewalSuccess","clientAccnum":"945282","clientSubacc":"0000","subscriptionId":"ccs_1","transactionId":"cct_1"}`)
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhookWithHeader(t, server.URL+"/v1/merchants/acme/webhooks/ccbill?eventType=RenewalSuccess", ccbillBody, "X-Unused", "unused"))

	globalNMIBody := []byte(`{"event_id":"evt_nmi_2","event_type":"transaction.sale.success","event_body":{"merchant":{"id":"nmi_acme_test"},"transaction_id":"txn_2"}}`)
	archivedNMIBody := []byte(`{"event_id":"evt_nmi_3","event_type":"transaction.sale.success","event_body":{"merchant":{"id":"nmi_acme_archived"},"transaction_id":"txn_3"}}`)
	globalCCBillBody := []byte(`{"eventType":"RenewalSuccess","clientAccnum":"945282","clientSubacc":"0000","subscriptionId":"ccs_2","transactionId":"cct_2"}`)
	require.Equal(t, http.StatusUnauthorized, postMerchantWebhook(t, server.URL+"/global/stripe/acct_acme_test", body, stripeSig("whsec_evil", ts, body)))
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhook(t, server.URL+"/global/stripe/acct_acme_test", body, stripeSig("whsec_acme_test", ts, body)))
	require.Equal(t, http.StatusUnauthorized, postMerchantWebhookWithHeader(t, server.URL+"/global/nmi", globalNMIBody, "Webhook-Signature", nmiSig("nmi_evil", ts, globalNMIBody)))
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhookWithHeader(t, server.URL+"/global/nmi", globalNMIBody, "Webhook-Signature", nmiSig("nmi_acme_test", ts, globalNMIBody)))
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhookWithHeader(t, server.URL+"/global/nmi", archivedNMIBody, "Webhook-Signature", nmiSig("nmi_archived", ts, archivedNMIBody)))
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhookWithHeader(t, server.URL+"/global/ccbill?eventType=RenewalSuccess", globalCCBillBody, "X-Unused", "unused"))
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

func seedRailMerchantAccount(t *testing.T, pool *pgxpool.Pool, merchantID, provider, accountID string) {
	seedRailMerchantAccountEnv(t, pool, merchantID, provider, "live", accountID)
}

func seedArchivedRailMerchantAccount(t *testing.T, pool *pgxpool.Pool, merchantID, provider, accountID string) {
	seedArchivedRailMerchantAccountEnv(t, pool, merchantID, provider, "live", accountID)
}

func seedArchivedRailMerchantAccountEnv(t *testing.T, pool *pgxpool.Pool, merchantID, provider, environment, accountID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO openrails.rail_merchant_accounts (merchant_id, rail, environment, account_id, archived)
		VALUES ($1::uuid, $2, $3, $4, true)
	`, merchantID, provider, environment, accountID)
	require.NoError(t, err)
}

func seedRailMerchantAccountEnv(t *testing.T, pool *pgxpool.Pool, merchantID, provider, environment, accountID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO openrails.rail_merchant_accounts (merchant_id, rail, environment, account_id, archived)
		VALUES ($1::uuid, $2, $3, $4, false)
	`, merchantID, provider, environment, accountID)
	require.NoError(t, err)
}

func putProviderSecret(t *testing.T, ctx context.Context, store merchants.MerchantSecretStore, merchantID merchant.ID, provider, accountID, key, value string) {
	putProviderSecretEnv(t, ctx, store, merchantID, provider, "live", accountID, key, value)
}

func putProviderSecretEnv(t *testing.T, ctx context.Context, store merchants.MerchantSecretStore, merchantID merchant.ID, provider, environment, accountID, key, value string) {
	t.Helper()
	name, err := merchants.RailMerchantAccountSecretName(provider, environment, accountID, key)
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
		CREATE TABLE IF NOT EXISTS openrails.rail_merchant_accounts (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			merchant_id uuid NOT NULL REFERENCES openrails.merchants(id) ON DELETE CASCADE,
			rail text NOT NULL,
			environment text NOT NULL DEFAULT 'live',
			account_id text NOT NULL,
			display_name text,
			archived boolean NOT NULL DEFAULT false,
			evidence jsonb,
			first_seen_at timestamptz NOT NULL DEFAULT current_timestamp,
			last_verified_at timestamptz,
			replaced_at timestamptz,
			created_at timestamptz NOT NULL DEFAULT current_timestamp,
			updated_at timestamptz NOT NULL DEFAULT current_timestamp
		);
		CREATE UNIQUE INDEX uq_rail_merchant_accounts_identity ON openrails.rail_merchant_accounts (rail, environment, account_id);
	`)
	require.NoError(t, err)
}
