//go:build integration

package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestMain(m *testing.M) {
	code := m.Run()
	dbtest.TerminateShared()
	os.Exit(code)
}

// #824: this test runs against the REAL migrated schema, connected as the
// unprivileged openrails_app role, so openrails.psps' ENABLE + FORCE'd
// merchant_isolation policy is live for every request the router makes.
//
// It used to build a BESPOKE schema (CREATE TABLE openrails.psps … with no RLS
// statements at all) on a superuser pool, which is why the #824 outage was
// invisible here: in production, resolving a webhook by provider account is a
// GUC-less read of a policy-bearing table, so it returned zero rows and no
// error and EVERY account-routed webhook answered 404. A harness that cannot
// reproduce the production posture cannot catch the regression.
func TestMerchantWebhookRouteHTTPResolvesMerchantBeforeVerifyingStripe(t *testing.T) {
	ctx := context.Background()
	pool, seed := newMerchantWebhookRoutePool(t)
	secrets := merchants.NewMemorySecretStore()
	// Deployment posture is test_mode (#681): the merchants service resolves
	// environment=test rows; live rows are seeded alongside to prove isolation.
	svc, err := merchants.NewService(db.WrapPool(pool, ""), secrets, "test")
	require.NoError(t, err)

	suffix := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	acmeSlug, evilSlug := "acme-"+suffix, "evil-"+suffix
	acme, _, err := svc.Provision(ctx, merchants.ProvisionRequest{Slug: acmeSlug, PermissionGroupID: "group-acme-" + suffix})
	require.NoError(t, err)
	evil, _, err := svc.Provision(ctx, merchants.ProvisionRequest{Slug: evilSlug, PermissionGroupID: "group-evil-" + suffix})
	require.NoError(t, err)

	// PSP identities are GLOBALLY unique (uq_rail_merchant_accounts_identity),
	// and this runs against the shared migrated database — so they carry the
	// run suffix. The CCBill pair keeps its clientAccnum-clientSubacc shape.
	acctAcme, acctEvil := "acct_acme_"+suffix, "acct_evil_"+suffix
	acctAcmeTest := "acct_acme_test_" + suffix
	nmiAcme, nmiEvil := "nmi_acme_account_"+suffix, "nmi_evil_account_"+suffix
	nmiAcmeTest, nmiArchived := "nmi_acme_test_"+suffix, "nmi_acme_archived_"+suffix
	ccbillAcct := "945282-0000"
	seedRailMerchantAccount(t, seed, acme.ID.String(), "stripe", acctAcme)
	seedRailMerchantAccount(t, seed, evil.ID.String(), "stripe", acctEvil)
	seedRailMerchantAccount(t, seed, acme.ID.String(), "nmi", nmiAcme)
	seedRailMerchantAccount(t, seed, evil.ID.String(), "nmi", nmiEvil)
	seedArchivedRailMerchantAccountEnv(t, seed, acme.ID.String(), "nmi", "test", nmiArchived)
	// NO live ccbill rows anywhere: the #668 test_mode IP-allowlist bypass is
	// refused while any environment=live ccbill account exists in the catalog.
	seedRailMerchantAccountEnv(t, seed, acme.ID.String(), "stripe", "test", acctAcmeTest)
	seedRailMerchantAccountEnv(t, seed, acme.ID.String(), "nmi", "test", nmiAcmeTest)
	seedRailMerchantAccountEnv(t, seed, acme.ID.String(), "ccbill", "test", ccbillAcct)
	putProviderSecret(t, ctx, secrets, acme.ID, "stripe", acctAcme, "webhook_signing_secret", "whsec_acme")
	putProviderSecret(t, ctx, secrets, evil.ID, "stripe", acctEvil, "webhook_signing_secret", "whsec_evil")
	putProviderSecret(t, ctx, secrets, acme.ID, "nmi", nmiAcme, "webhook_signing_secret", "nmi_acme")
	putProviderSecret(t, ctx, secrets, evil.ID, "nmi", nmiEvil, "webhook_signing_secret", "nmi_evil")
	putProviderSecretEnv(t, ctx, secrets, acme.ID, "nmi", "test", nmiArchived, "webhook_signing_secret", "nmi_archived")
	putProviderSecretEnv(t, ctx, secrets, acme.ID, "stripe", "test", acctAcmeTest, "webhook_signing_secret", "whsec_acme_test")
	putProviderSecretEnv(t, ctx, secrets, acme.ID, "nmi", "test", nmiAcmeTest, "webhook_signing_secret", "nmi_acme_test")

	// SEC-19: the CCBill source-IP gate has no blanket test_mode bypass — the
	// httptest client's loopback address must be DECLARED, and is still only
	// honored because the probe above proves no live ccbill PSP exists.
	sandboxCfg := func() *config.Config {
		return &config.Config{
			TestMode:                 config.CredentialPostureSandbox,
			CCBillWebhookIPAllowlist: []string{"127.0.0.1/32", "::1/128"},
		}
	}
	rt := &app.Runtime{Config: sandboxCfg(), Merchants: svc}
	globalRT := &app.Runtime{Config: sandboxCfg(), Merchants: svc}
	mux := http.NewServeMux()
	httproutes.RegisterWebhookRoutes(router.NewMux(mux, "/global", globalRT), globalRT)
	httproutes.RegisterMerchantWebhookRoutes(router.NewMux(mux, "/v1", rt), rt)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	body := []byte(`{"id":"evt_1","type":"checkout.session.completed"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	require.Equal(t, http.StatusNotFound, postMerchantWebhook(t, server.URL+"/v1/merchants/nope/webhooks/stripe", body, stripeSig("whsec_acme_test", ts, body)))
	require.Equal(t, http.StatusUnauthorized, postMerchantWebhook(t, server.URL+"/v1/merchants/"+acmeSlug+"/webhooks/stripe", body, stripeSig("whsec_evil", ts, body)))
	// test_mode posture resolves the environment=test account's secret (#681);
	// the live account's secret no longer verifies.
	require.Equal(t, http.StatusUnauthorized, postMerchantWebhook(t, server.URL+"/v1/merchants/"+acmeSlug+"/webhooks/stripe", body, stripeSig("whsec_acme", ts, body)))
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhook(t, server.URL+"/v1/merchants/"+acmeSlug+"/webhooks/stripe", body, stripeSig("whsec_acme_test", ts, body)))

	nmiBody := []byte(`{"event_id":"evt_nmi_1","event_type":"transaction.sale.success","event_body":{"merchant":{"id":"` + nmiAcmeTest + `"},"transaction_id":"txn_1"}}`)
	require.Equal(t, http.StatusUnauthorized, postMerchantWebhookWithHeader(t, server.URL+"/v1/merchants/"+acmeSlug+"/webhooks/nmi", nmiBody, "Webhook-Signature", nmiSig("nmi_evil", ts, nmiBody)))
	require.Equal(t, http.StatusUnauthorized, postMerchantWebhookWithHeader(t, server.URL+"/v1/merchants/"+acmeSlug+"/webhooks/nmi", nmiBody, "Webhook-Signature", nmiSig("nmi_acme", ts, nmiBody)))
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhookWithHeader(t, server.URL+"/v1/merchants/"+acmeSlug+"/webhooks/nmi", nmiBody, "Webhook-Signature", nmiSig("nmi_acme_test", ts, nmiBody)))

	// or#893 phase 6: /webhooks/mobius is gone. It is a PSP key, not a rail —
	// and a VALIDLY signed body must still be refused, at the route, before any
	// secret is loaded.
	status, refusal := postMerchantWebhookBody(t, server.URL+"/v1/merchants/"+acmeSlug+"/webhooks/mobius", nmiBody, "Webhook-Signature", nmiSig("nmi_acme_test", ts, nmiBody))
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, refusal, "/webhooks/mobius was removed (or#893) — post to /webhooks/nmi")

	// or#893 phase 6: ONE NMI signature header. The three speculative X-…
	// spellings are no longer read, so a correctly signed body presented under
	// them is unsigned.
	for _, retired := range []string{"X-Signature", "X-NMI-Signature", "X-Mobius-Signature"} {
		require.Equal(t, http.StatusUnauthorized,
			postMerchantWebhookWithHeader(t, server.URL+"/v1/merchants/"+acmeSlug+"/webhooks/nmi", nmiBody, retired, nmiSig("nmi_acme_test", ts, nmiBody)),
			retired)
	}

	ccbillBody := []byte(`{"eventType":"RenewalSuccess","clientAccnum":"945282","clientSubacc":"0000","subscriptionId":"ccs_1","transactionId":"cct_1"}`)
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhookWithHeader(t, server.URL+"/v1/merchants/"+acmeSlug+"/webhooks/ccbill?eventType=RenewalSuccess", ccbillBody, "X-Unused", "unused"))

	globalNMIBody := []byte(`{"event_id":"evt_nmi_2","event_type":"transaction.sale.success","event_body":{"merchant":{"id":"` + nmiAcmeTest + `"},"transaction_id":"txn_2"}}`)
	archivedNMIBody := []byte(`{"event_id":"evt_nmi_3","event_type":"transaction.sale.success","event_body":{"merchant":{"id":"` + nmiArchived + `"},"transaction_id":"txn_3"}}`)
	globalCCBillBody := []byte(`{"eventType":"RenewalSuccess","clientAccnum":"945282","clientSubacc":"0000","subscriptionId":"ccs_2","transactionId":"cct_2"}`)
	require.Equal(t, http.StatusUnauthorized, postMerchantWebhook(t, server.URL+"/global/stripe/"+acctAcmeTest, body, stripeSig("whsec_evil", ts, body)))
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhook(t, server.URL+"/global/stripe/"+acctAcmeTest, body, stripeSig("whsec_acme_test", ts, body)))
	require.Equal(t, http.StatusUnauthorized, postMerchantWebhookWithHeader(t, server.URL+"/global/nmi", globalNMIBody, "Webhook-Signature", nmiSig("nmi_evil", ts, globalNMIBody)))
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhookWithHeader(t, server.URL+"/global/nmi", globalNMIBody, "Webhook-Signature", nmiSig("nmi_acme_test", ts, globalNMIBody)))
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhookWithHeader(t, server.URL+"/global/nmi", archivedNMIBody, "Webhook-Signature", nmiSig("nmi_archived", ts, archivedNMIBody)))
	require.Equal(t, http.StatusInternalServerError, postMerchantWebhookWithHeader(t, server.URL+"/global/ccbill?eventType=RenewalSuccess", globalCCBillBody, "X-Unused", "unused"))
}

func postMerchantWebhook(t *testing.T, url string, body []byte, sig string) int {
	return postMerchantWebhookWithHeader(t, url, body, "Stripe-Signature", sig)
}

func postMerchantWebhookBody(t *testing.T, url string, body []byte, header string, sig string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(header, sig)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(raw)
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
		INSERT INTO openrails.psps (merchant_id, rail, environment, account_id, archived)
		VALUES ($1::uuid, $2, $3, $4, true)
	`, merchantID, provider, environment, accountID)
	require.NoError(t, err)
}

func seedRailMerchantAccountEnv(t *testing.T, pool *pgxpool.Pool, merchantID, provider, environment, accountID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO openrails.psps (merchant_id, rail, environment, account_id, archived)
		VALUES ($1::uuid, $2, $3, $4, false)
	`, merchantID, provider, environment, accountID)
	require.NoError(t, err)
}

func putProviderSecret(t *testing.T, ctx context.Context, store merchants.MerchantSecretStore, merchantID merchant.ID, provider, accountID, key, value string) {
	putProviderSecretEnv(t, ctx, store, merchantID, provider, "live", accountID, key, value)
}

func putProviderSecretEnv(t *testing.T, ctx context.Context, store merchants.MerchantSecretStore, merchantID merchant.ID, provider, environment, accountID, key, value string) {
	t.Helper()
	name, err := merchants.PSPSecretName(provider, environment, accountID, key)
	require.NoError(t, err)
	_, err = store.Put(ctx, merchantID, name, value)
	require.NoError(t, err)
}

func nmiSig(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return fmt.Sprintf("t=%s,s=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// newMerchantWebhookRoutePool returns the pool the ROUTER runs on and a
// separate seeding pool.
//
// #824: the router's pool connects as openrails_app — NOBYPASSRLS, exactly what
// production must use — against the fully migrated schema, so every policy is
// live. Seeding merchant-owned rows (psps) needs cross-merchant writes that RLS
// correctly forbids, so fixtures go through the superuser pool. The two roles
// are the whole point of the harness: previously both were the same superuser
// connection against a hand-written, policy-free table.
func newMerchantWebhookRoutePool(t *testing.T) (app *pgxpool.Pool, seed *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	superDSN, appDSN := dbtest.SharedRLSPostgres(t)

	seed, err := pgxpool.New(ctx, superDSN)
	require.NoError(t, err)
	t.Cleanup(seed.Close)

	app, err = pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	t.Cleanup(app.Close)

	var enforcing bool
	require.NoError(t, app.QueryRow(ctx, `
		SELECT NOT COALESCE(bool_or(rolsuper OR rolbypassrls), TRUE)
		  FROM pg_roles WHERE rolname = current_user`).Scan(&enforcing))
	require.True(t, enforcing, "the webhook router must be exercised under an RLS-ENFORCING role, or #824 is invisible again")
	return app, seed
}
