//go:build integration

package embed_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #788 end-to-end pins for the merchant-scoped CCBill webhook leg: the
// dispatcher builds its CCBill client PER MERCHANT at dispatch time from the
// armed rail state (rail_merchant_accounts + scoped secrets) — the SAME
// resolution seam regardless of whether MODE 1 (manifest) or MODE 2 (API)
// armed it — and an unarmed rail fails closed.

// ccbillWebhookTestPriceMicros is the seeded catalog price; the webhook's
// billed amounts must match it (validateCCBillBilledAmount).
const ccbillWebhookTestPriceMicros = 9_990_000 // $9.99

func sandboxModeConfig(dsn string, source string) *config.Config {
	return &config.Config{
		Env: "dev",
		// Sandbox posture + an EXPLICIT loopback allowlist entry. SEC-19 replaced
		// the old implicit "test_mode accepts any IP" bypass: the extra CIDR is a
		// declared credential, honored only under sandbox posture and only while
		// the PSP catalog proves no live CCBill PSP exists. httptest posts from
		// loopback, so the harness must declare it — same as hentai0's compose
		// suite and internal/http's merchant-webhook suite.
		TestMode:                 config.CredentialPostureSandbox,
		CCBillWebhookIPAllowlist: []string{"127.0.0.1/32", "::1/128"},
		MerchantSource:           source,
		ProviderWriteMode:        config.ProviderWriteModeFull,
		DB:                       &config.DBConfig{URL: dsn},
	}
}

// seedCCBillWebhookCatalog pushes one product with a ccbill-linked price and
// returns the flex id + form name the webhook payload must carry.
func seedCCBillWebhookCatalog(t *testing.T, ctx context.Context, cfg *config.Config, slug string) (flexID, formName string) {
	t.Helper()
	flexID = uuid.NewString()
	formName = "test-form"
	raw := []byte(fmt.Sprintf(`version: 1
catalogs:
  - merchant: %s
    products:
      - key: pro-%s
        display_name: Pro
        entitlements: [pro-access]
        prices:
          - currency: usd
            unit_amount: %d
            duration: 30d
            auto_renew: true
            psps: [ccbill]
            psp_links:
              ccbill:
                flex_id: %q
                form_name: %q
`, slug, slug, ccbillWebhookTestPriceMicros, flexID, formName))
	require.NoError(t, embedded.PushMerchantCatalog(ctx, embedded.CatalogPushOptions{
		Config:   cfg,
		Manifest: raw,
		Insert:   true, Overwrite: true, Prune: true,
	}))
	return flexID, formName
}

// seedProfileUser inserts the profiles.users row the webhook's username
// resolution reads (AuthKit-managed identity in production).
func seedProfileUser(t *testing.T, ctx context.Context, dsn, username string) (userID string) {
	t.Helper()
	appDB := dbtest.OpenAppDB(t, dsn)
	userID = uuid.NewString()
	now := time.Now().UTC()
	_, err := appDB.Pool().Exec(ctx, `
		INSERT INTO profiles.users (id, username, email, email_verified, created_at, updated_at)
		VALUES ($1, $2, $3, true, $4, $4)
		ON CONFLICT (id) DO UPDATE SET username = EXCLUDED.username, updated_at = EXCLUDED.updated_at
	`, userID, username, username+"@test.example.com", now)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = appDB.Pool().Exec(context.Background(), `DELETE FROM profiles.users WHERE id = $1`, userID)
	})
	return userID
}

func ccbillNewSalePayload(accountID, flexID, formName, username, reservationID, subID, txnID string) map[string]any {
	accnum, subacc, _ := strings.Cut(accountID, "-")
	return map[string]any{
		"eventType":                  "NewSaleSuccess",
		"subscriptionId":             subID,
		"transactionId":              txnID,
		"clientAccnum":               accnum,
		"clientSubacc":               subacc,
		"timestamp":                  time.Now().UTC().Format("2006-01-02 15:04:05"),
		"firstName":                  "Integration",
		"lastName":                   "Webhook",
		"address1":                   "123 Test St",
		"city":                       "Denver",
		"state":                      "CO",
		"country":                    "US",
		"postalCode":                 "80202",
		"email":                      username + "@test.example.com",
		"username":                   username,
		"formName":                   formName,
		"flexId":                     flexID,
		"billedInitialPrice":         "9.99",
		"billedRecurringPrice":       "9.99",
		"billedCurrencyCode":         "USD",
		"subscriptionInitialPrice":   "9.99",
		"subscriptionRecurringPrice": "9.99",
		"subscriptionCurrencyCode":   "USD",
		"nextRenewalDate":            time.Now().UTC().Add(30 * 24 * time.Hour).Format("2006-01-02"),
		"reservationId":              reservationID,
		"paymentType":                "CREDIT",
		"cardType":                   "VISA",
		"last4":                      "1111",
		"expDate":                    "1228",
	}
}

func postCCBillMerchantWebhook(t *testing.T, serverURL, slug string, payload map[string]any) (int, string) {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost,
		serverURL+"/v1/merchants/"+slug+"/webhooks/ccbill?eventType="+payload["eventType"].(string),
		strings.NewReader(string(body)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(raw)
}

// assertCCBillSubscriptionActive reads under the MERCHANT's own scope. The
// default integration handle is the RLS-enforcing openrails_app role, so an
// unpinned read of any merchant-owned table matches zero rows and reports the
// webhook as having written nothing.
func assertCCBillSubscriptionActive(t *testing.T, ctx context.Context, mid merchant.ID, railSubID string) {
	t.Helper()
	appDB := dbtest.OpenMerchantDB(t, mid.UUID())
	var status string
	require.NoError(t, appDB.Pool().QueryRow(ctx,
		`SELECT status FROM openrails.subscriptions WHERE merchant_id = $1 AND rail = 'ccbill' AND rail_subscription_id = $2`,
		mid.UUID(), railSubID).Scan(&status), "webhook must create the ccbill subscription")
	require.Equal(t, "active", status)
}

// cleanupCCBillWebhookMerchant deletes under the merchant's own scope — an
// unpinned DELETE matches zero rows under RLS and leaks every fixture row into
// the shared database without erroring.
func cleanupCCBillWebhookMerchant(t *testing.T, mid merchant.ID) {
	t.Helper()
	appDB := dbtest.OpenMerchantDB(t, mid.UUID())
	t.Cleanup(func() {
		pool := appDB.Pool()
		for _, stmt := range []string{
			`DELETE FROM openrails.webhook_events WHERE merchant_id = $1`,
			`DELETE FROM openrails.entitlements WHERE merchant_id = $1`,
			`DELETE FROM openrails.access_windows WHERE merchant_id = $1`,
			`DELETE FROM openrails.payments WHERE merchant_id = $1`,
			`DELETE FROM openrails.checkout_sessions WHERE merchant_id = $1`,
			`DELETE FROM openrails.subscriptions WHERE merchant_id = $1`,
			`DELETE FROM openrails.customers WHERE merchant_id = $1`,
			`DELETE FROM openrails.prices WHERE merchant_id = $1`,
			`DELETE FROM openrails.products WHERE merchant_id = $1`,
			`DELETE FROM openrails.merchant_secrets WHERE merchant_id = $1`,
			`DELETE FROM openrails.psps WHERE merchant_id = $1`,
			`DELETE FROM openrails.merchants WHERE id = $1`,
		} {
			_, _ = pool.Exec(context.Background(), stmt, mid.UUID())
		}
	})
}

// TestManifestMode_CCBillWebhookNewSaleSuccessEndToEnd is the hentai0 shape
// (#788): MODE 1 manifest-armed ccbill account, checkout session opened over
// the embedded customer surface, then the merchant-scoped NewSaleSuccess
// webhook processed synchronously — accepted, subscription created, session
// marked succeeded. Before #788 this leg 500'd "ccbill rest client not
// configured": the dispatcher only knew the boot-config bridge.
func TestManifestMode_CCBillWebhookNewSaleSuccessEndToEnd(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("mwhe2e%d", nano)
	ccbillAccount := fmt.Sprintf("94%04d-0001", nano%10_000)

	cfg := sandboxModeConfig(dsn, config.MerchantSourceManifest)
	rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	id, err := rt.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{
		DisplayName: slug,
		PSPs: map[string]embed.PSPConfig{
			"ccbill": {
				"ccbill": {
					Environment: "test",
					AccountID:   ccbillAccount,
					Secrets:     map[string]string{"salt": "test-salt-" + slug},
				},
			},
		},
	})
	require.NoError(t, err)
	cleanupCCBillWebhookMerchant(t, id)

	flexID, formName := seedCCBillWebhookCatalog(t, ctx, cfg, slug)
	username := "ccbill_e2e_" + uuid.NewString()[:8]
	userID := seedProfileUser(t, ctx, dsn, username)

	email := username + "@test.example.com"
	userAuthn := billingauth.AuthenticatorFunc(func(context.Context, *http.Request) (billingauth.UserContext, error) {
		return billingauth.UserContext{UserID: userID, Email: email, EmailVerified: true}, nil
	})
	delegated := billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
		return &billingauth.DelegatedPrincipal{MerchantID: id.UUID().String(), SubjectID: userID, Email: email, EmailVerified: true, Username: username}, nil
	})
	handler, err := embedded.MountHandler(rt.Embedded(), embedded.MountOptions{
		RouteSets:              []embed.RouteSet{embed.RouteSetCheckout, embed.RouteSetCustomer, embed.RouteSetWebhooks},
		Authenticator:          userAuthn,
		DelegatedAuthenticator: delegated,
	})
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// Resolve the seeded price id over the public catalog surface.
	priceID := fetchCCBillPriceID(t, server.URL)

	// Open a checkout session for the ccbill rail (requires_action: the buyer
	// is redirected to the FlexForm; the webhook closes the loop).
	sessionID := openCCBillCheckoutSession(t, server.URL, priceID, username)

	subID := fmt.Sprintf("09%d", nano%1_000_000_000)
	txnID := fmt.Sprintf("19%d", nano%1_000_000_000)
	payload := ccbillNewSalePayload(ccbillAccount, flexID, formName, username, sessionID, subID, txnID)

	status, body := postCCBillMerchantWebhook(t, server.URL, slug, payload)
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, `"status":"accepted"`)

	assertCCBillSubscriptionActive(t, ctx, id, subID)

	// The reservation loop closes: the checkout session flips to succeeded.
	appDB := dbtest.OpenMerchantDB(t, id.UUID())
	var sessionStatus string
	require.NoError(t, appDB.Pool().QueryRow(ctx,
		`SELECT status FROM openrails.checkout_sessions WHERE merchant_id = $1`, id.UUID()).Scan(&sessionStatus))
	require.Equal(t, "succeeded", sessionStatus, "NewSaleSuccess must mark the reservation succeeded")
}

// TestAPIMode_CCBillWebhookNewSaleSuccessEndToEnd is the MODE-2 twin: the SAME
// webhook leg with the ccbill account armed through the management API
// (PUT /v1/merchant/payment-providers/ccbill) instead of a manifest. The
// dispatcher must be mode-blind: it resolves the same armed state.
func TestAPIMode_CCBillWebhookNewSaleSuccessEndToEnd(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("mwhapi%d", nano)
	ccbillAccount := fmt.Sprintf("95%04d-0002", nano%10_000)

	cfg := sandboxModeConfig(dsn, config.MerchantSourceAPI)
	rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	// API mode: bare identity bind; rail truth arrives over the HTTP API.
	id, err := rt.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{DisplayName: slug})
	require.NoError(t, err)
	require.NoError(t, rt.Embedded().App().Runtime.EnsureMerchantsService(ctx))
	cleanupCCBillWebhookMerchant(t, id)

	handler, err := embedded.MountHandler(rt.Embedded(), embedded.MountOptions{
		RouteSets:      []embed.RouteSet{embed.RouteSetPaymentProviders, embed.RouteSetCatalog},
		Gate:           allowAllGate{id: id},
		ProviderRoutes: &embedded.ProviderRoutes{Webhooks: true},
	})
	require.NoError(t, err)
	adminServer := httptest.NewServer(handler)
	t.Cleanup(adminServer.Close)

	// Layer A, MODE 2: arm the ccbill account over the management API.
	payload := fmt.Sprintf(`{"environment":"test","account_id":%q,"credentials":{"salt":"api-salt-%s"}}`, ccbillAccount, slug)
	req, err := http.NewRequest(http.MethodPut, adminServer.URL+"/v1/merchant/payment-providers/ccbill", strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	// Layer A, MODE 2: the catalog also arrives over the API (a manifest push
	// is refused as a second truth) — publish a ccbill-linked price.
	flexID := uuid.NewString()
	formName := "test-form"
	publish := fmt.Sprintf(`{"insert":true,"overwrite":true,"catalog":{"version":1,"products":[{"key":"pro-%s","display_name":"Pro","entitlements":["pro-access"],"prices":[{"currency":"usd","unit_amount":%d,"duration":"30d","auto_renew":true,"psps":["ccbill"],"psp_links":{"ccbill":{"flex_id":%q,"form_name":%q}}}]}]}}`,
		slug, ccbillWebhookTestPriceMicros, flexID, formName)
	req, err = http.NewRequest(http.MethodPost, adminServer.URL+"/v1/merchant/catalog/publish", strings.NewReader(publish))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	// The published price must exist with its ccbill link before the webhook.
	appDB := dbtest.OpenMerchantDB(t, id.UUID())
	var priceCount int
	require.NoError(t, appDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM openrails.prices WHERE merchant_id = $1 AND psp_links -> 'ccbill' ->> 'flex_id' = $2`,
		id.UUID(), flexID).Scan(&priceCount))
	require.Equal(t, 1, priceCount, "publish response: %s", string(raw))

	username := "ccbill_api_" + uuid.NewString()[:8]
	seedProfileUser(t, ctx, dsn, username)

	// Webhook-only mount (the ingestion surface a MODE-2 host exposes).
	webhookHandler, err := embedded.MountHandler(rt.Embedded(), embedded.MountOptions{
		RouteSets: []embed.RouteSet{embed.RouteSetWebhooks},
	})
	require.NoError(t, err)
	webhookServer := httptest.NewServer(webhookHandler)
	t.Cleanup(webhookServer.Close)

	subID := fmt.Sprintf("29%d", nano%1_000_000_000)
	txnID := fmt.Sprintf("39%d", nano%1_000_000_000)
	event := ccbillNewSalePayload(ccbillAccount, flexID, formName, username, "", subID, txnID)

	status, body := postCCBillMerchantWebhook(t, webhookServer.URL, slug, event)
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, `"status":"accepted"`)

	assertCCBillSubscriptionActive(t, ctx, id, subID)
}

// TestCCBillWebhookUnarmedRailFailsClosed (#788): a merchant with NO armed
// ccbill account must REJECT the webhook (5xx — the provider redelivers once
// armed), never ack-and-drop and never default-allow processing.
func TestCCBillWebhookUnarmedRailFailsClosed(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("mwhoff%d", nano)

	cfg := sandboxModeConfig(dsn, config.MerchantSourceManifest)
	rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	// Merchant exists but declares NO rail accounts at all.
	id, err := rt.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{DisplayName: slug})
	require.NoError(t, err)
	cleanupCCBillWebhookMerchant(t, id)

	username := "ccbill_off_" + uuid.NewString()[:8]
	seedProfileUser(t, ctx, dsn, username)

	// Force the webhook route mounted (the armed-account derivation would
	// drop it) so the DISPATCHER's fail-closed rejection is what answers.
	handler, err := embedded.MountHandler(rt.Embedded(), embedded.MountOptions{
		RouteSets:      []embed.RouteSet{embed.RouteSetWebhooks},
		ProviderRoutes: &embedded.ProviderRoutes{Webhooks: true},
	})
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	payload := ccbillNewSalePayload("945299-0000", uuid.NewString(), "test-form", username, "", "0999", "1999")
	status, body := postCCBillMerchantWebhook(t, server.URL, slug, payload)
	require.Equal(t, http.StatusInternalServerError, status, "unarmed rail must reject, never accept: %s", body)
	require.NotContains(t, body, "accepted")

	// Fail closed means NOTHING was processed.
	appDB := dbtest.OpenMerchantDB(t, id.UUID())
	var n int
	require.NoError(t, appDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM openrails.subscriptions WHERE merchant_id = $1`, id.UUID()).Scan(&n))
	require.Zero(t, n, "no subscription may be created from an unarmed rail's webhook")
}

// fetchCCBillPriceID resolves the seeded catalog's price id over the public
// catalog surface (same as hentai0's harness does).
func fetchCCBillPriceID(t *testing.T, serverURL string) string {
	t.Helper()
	resp, err := http.Get(serverURL + "/v1/prices")
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &out), string(raw))
	require.NotEmpty(t, out.Data, "seeded price must be listed: %s", string(raw))
	return out.Data[0].ID
}

// openCCBillCheckoutSession opens a ccbill checkout session over the embedded
// customer surface and returns its id (status requires_action).
func openCCBillCheckoutSession(t *testing.T, serverURL, priceID, username string) string {
	t.Helper()
	body := fmt.Sprintf(`{"price_id":%q,"mode":"subscription","payment":{"rail":"ccbill","email":%q,"first_name":"Integration","last_name":"Webhook","address1":"123 Test St","city":"Denver","state":"CO","zip":"80202","country":"US"}}`,
		priceID, username+"@test.example.com")
	req, err := http.NewRequest(http.MethodPost, serverURL+"/v1/me/checkout", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer host-credential")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var session struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(raw, &session))
	require.NotEmpty(t, session.ID)
	require.Equal(t, "requires_action", session.Status, string(raw))
	return session.ID
}
