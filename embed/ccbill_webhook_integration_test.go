//go:build integration

package embed_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/open-rails/openrails"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/app"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
	billingauthkit "github.com/open-rails/openrails/internal/hostauth"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/open-rails/openrails/internal/httptesthost"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #788 end-to-end pins for the merchant-scoped CCBill webhook leg: the
// dispatcher builds its CCBill client PER MERCHANT at dispatch time from the
// armed rail state (psps + scoped secrets) — the SAME
// resolution seam regardless of whether MODE 1 (manifest) or MODE 2 (API)
// armed it — and an unarmed rail fails closed.

// ccbillWebhookTestPriceMicros is the seeded catalog price; the webhook's
// billed amounts must match it (validateCCBillBilledAmount).
const ccbillWebhookTestPriceMicros = 9_990_000 // $9.99

func sandboxModeConfig(dsn string, source string) *config.Config {
	return &config.Config{
		// Sandbox posture + an EXPLICIT loopback allowlist entry. SEC-19 replaced
		// the old implicit "test_mode accepts any IP" bypass: the extra CIDR is a
		// declared credential, honored only under sandbox posture and only while
		// the PSP catalog proves no live CCBill PSP exists. httptest posts from
		// loopback, so the harness must declare it — same as host-two's compose
		// suite and internal/http's merchant-webhook suite.
		TestMode:                 config.CredentialPostureSandbox,
		CCBillWebhookIPAllowlist: []string{"127.0.0.1/32", "::1/128"},
		SecretBackend:            source,
		AllowCatalogUpdates:      true,
		// or#893: merchant_config_source=api declares where secrets live. MODE 1 never
		// consults it, so db is inert there and honest in MODE 2.
		ProviderWriteMode: config.ProviderWriteModeFull,
		DB:                &config.DBConfig{URL: dsn},
	}
}

// seedCCBillWebhookCatalog pushes one product with a ccbill-linked price and
// returns the flex id + form name the webhook payload must carry.
func seedCCBillWebhookCatalog(t *testing.T, ctx context.Context, cfg *config.Config, slug string) (flexID, formName string) {
	t.Helper()
	flexID = uuid.NewString()
	formName = "test-form"
	raw := []byte(fmt.Sprintf(`schema_version: 1
application_id: ccbill-webhook-%s
expected_revision: 0
products:
  - key: pro-%s
    display_name: Pro
    entitlements_spec: {pro-access: null}
    prices:
      - key: pro-%s-monthly
        currency: USD
        unit_amount: %d
        access_duration_hours: 720
        auto_renew: true
        psps: [ccbill]
        psp_links:
          ccbill:
            flex_id: %q
            form_name: %q
`, slug, slug, slug, ccbillWebhookTestPriceMicros, flexID, formName))
	_, err := hosttools.ApplyMerchantCatalog(ctx, hosttools.CatalogApplyOptions{Config: cfg, Merchant: slug, Manifest: raw})
	require.NoError(t, err)

	return flexID, formName
}

// ccbillIdentity explicitly selects AuthKit as this host's identity provider.
func ccbillIdentity(t *testing.T, ctx context.Context, dsn string) authkit.Client {
	t.Helper()
	appDB := dbtest.OpenAppDB(t, dsn)
	core, err := authcore.New(authcore.Config{
		Keys:      authcore.KeysConfig{VerifyOnly: true},
		Token:     authcore.TokenConfig{Issuer: "https://ccbill.test", IssuedAudiences: []string{"test"}},
		Ephemeral: authcore.EphemeralConfig{AllowMemory: true},
	}, authcore.Deps{Postgres: appDB.Pool()})
	require.NoError(t, err)
	t.Cleanup(core.Close)
	return core.Client()
}

func seedProfileUser(t *testing.T, ctx context.Context, dsn, username string) string {
	t.Helper()
	core := ccbillIdentity(t, ctx, dsn)
	user, err := core.CreateUser(ctx, username+"@test.example.com", username)
	require.NoError(t, err)
	// Each fixture has a unique username in dbtest's owned scratch database.
	// Its final database cleanup also removes identities; no lifecycle jobs
	// or privileged per-user deletion are needed during test teardown.
	return user.ID
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
		serverURL+"/v1/webhooks/ccbill/"+payload["clientAccnum"].(string)+"-"+payload["clientSubacc"].(string)+"?eventType="+payload["eventType"].(string),
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
		`SELECT status FROM billing.subscriptions WHERE merchant_id = $1 AND rail = 'ccbill' AND rail_subscription_id = $2`,
		mid.UUID(), railSubID).Scan(&status), "webhook must create the ccbill subscription")
	require.Equal(t, "active", status)
}

// Deactivate this test's unique merchant without deleting financial history.
// Immutable subscription transitions and grants survive until dbtest drops
// this process's owned scratch database after all tests finish.
func cleanupCCBillWebhookMerchant(t *testing.T, mid merchant.ID) {
	t.Helper()
	appDB := dbtest.OpenAppDB(t, dbtest.SharedSuperuserDSN(t))
	t.Cleanup(func() {
		updated, err := appDB.Pool().Exec(context.Background(),
			`UPDATE billing.merchants SET status='deleted', deleted_at=now(), updated_at=now() WHERE id=$1`, mid.UUID())
		require.NoError(t, err, "deactivate owned merchant fixture")
		require.EqualValues(t, 1, updated.RowsAffected())
	})
}

// TestManifestMode_CCBillWebhookNewSaleSuccessEndToEnd is the host-two shape
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

	cfg := sandboxModeConfig(dsn, config.SecretBackendSnapshot)

	rt, id, err := newDeclaredMerchant(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails(), UsernameResolver: billingauthkit.NewDirectory(ccbillIdentity(t, ctx, dsn))}, slug, embed.MerchantConfig{
		DisplayName: slug,
		PSPs: map[string]embed.PSPConfig{
			"ccbill": {
				"ccbill": {
					AccountID: ccbillAccount,
					Secrets:   map[string]string{"salt": "test-salt-" + slug},
				},
			},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
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
	handler, err := httptesthost.Handler(rt, httptesthost.Options{HTTP: embed.HTTPConfig{Checkout: true, CustomerRoutes: []embed.CustomerRoutesConfig{{Treasury: true}}}, DelegatedAuthenticator: delegated, Authenticator: userAuthn})
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// Resolve the seeded price id over the public catalog surface.
	priceID := fetchCCBillPriceID(t, server.URL)

	// A provider-owned checkout started before the new-enrollment hard cut
	// still completes through its ordinary provider webhook.
	sessionID := seedLegacyCCBillCheckoutSession(t, ctx, id, priceID, userID)

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
		`SELECT status FROM billing.checkout_sessions WHERE merchant_id = $1`, id.UUID()).Scan(&sessionStatus))
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

	cfg := sandboxModeConfig(dsn, config.SecretBackendDB)
	cfg.Encryption = &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}
	// API mode: bare identity bind; rail truth arrives over the HTTP API.

	rt, id, err := newDeclaredMerchant(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails(), UsernameResolver: billingauthkit.NewDirectory(ccbillIdentity(t, ctx, dsn))}, slug, embed.MerchantConfig{DisplayName: slug})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	require.NoError(t, app.HostGraph(rt).Runtime.EnsureMerchantsService(ctx))
	cleanupCCBillWebhookMerchant(t, id)

	handler, err := httptesthost.Handler(rt, httptesthost.Options{HTTP: embed.HTTPConfig{MerchantConfig: true, Catalog: true}, Gate: allowAllGate{id: id}})
	require.NoError(t, err)
	adminServer := httptest.NewServer(handler)
	t.Cleanup(adminServer.Close)

	// Layer A, MODE 2: arm the ccbill account over the management API.
	payload := fmt.Sprintf(`{"operation_id":%q,"expected_revision":0,"account_id":%q,"credentials":{"salt":"api-salt-%s"}}`, uuid.NewString(), ccbillAccount, slug)
	req, err := http.NewRequest(http.MethodPut, adminServer.URL+"/v1/merchant/payment-providers/ccbill", strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	// Catalog authoring uses ordinary item APIs; provider credential custody
	// remains independent of this writable catalog.
	flexID := uuid.NewString()
	formName := "test-form"
	client, err := rt.Client()
	require.NoError(t, err)
	product, err := client.Products.Create(ctx, &openrails.ProductCreateParams{
		Key: "pro-" + slug, DisplayName: "Pro", EntitlementsSpec: map[string]*int{"pro-access": nil},
	})
	require.NoError(t, err)
	duration := 720
	_, err = client.Prices.Create(ctx, &openrails.PriceCreateParams{
		ProductID: product.ID, Key: "pro-" + slug + "-monthly", UnitAmount: ccbillWebhookTestPriceMicros, Currency: "USD",
		AccessDurationHours: &duration, AutoRenew: true, PSPs: []string{"ccbill"},
		PSPLinks: map[string]map[string]string{"ccbill": {"flex_id": flexID, "form_name": formName}},
	})
	require.NoError(t, err)

	// The published price must exist with its ccbill link before the webhook.
	appDB := dbtest.OpenMerchantDB(t, id.UUID())
	var priceCount int
	require.NoError(t, appDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM billing.price_psp_bindings WHERE merchant_id = $1 AND flex_id = $2`,
		id.UUID(), flexID).Scan(&priceCount))
	require.Equal(t, 1, priceCount, "publish response: %s", string(raw))

	username := "ccbill_api_" + uuid.NewString()[:8]
	seedProfileUser(t, ctx, dsn, username)

	// Webhook-only mount (the ingestion surface a MODE-2 host exposes).
	webhookHandler, err := httptesthost.Handler(rt, httptesthost.Options{HTTP: embed.HTTPConfig{}})
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
// ccbill account must REJECT the explicitly addressed unknown account (404),
// never ack-and-drop and never default-allow processing.
func TestCCBillWebhookUnarmedRailFailsClosed(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("mwhoff%d", nano)

	cfg := sandboxModeConfig(dsn, config.SecretBackendSnapshot)
	// Merchant exists but declares NO rail accounts at all.

	rt, id, err := newDeclaredMerchant(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails(), UsernameResolver: billingauthkit.NewDirectory(ccbillIdentity(t, ctx, dsn))}, slug, embed.MerchantConfig{DisplayName: slug})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	cleanupCCBillWebhookMerchant(t, id)

	username := "ccbill_off_" + uuid.NewString()[:8]
	seedProfileUser(t, ctx, dsn, username)

	// The callback route exists independently of accounts, but its account
	// lookup must refuse this unknown account before invoking the dispatcher.
	handler, err := httptesthost.Handler(rt, httptesthost.Options{HTTP: embed.HTTPConfig{}})
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	payload := ccbillNewSalePayload("945299-0000", uuid.NewString(), "test-form", username, "", "0999", "1999")
	status, body := postCCBillMerchantWebhook(t, server.URL, slug, payload)
	require.Equal(t, http.StatusNotFound, status, "unknown account must reject, never accept: %s", body)
	require.NotContains(t, body, "accepted")

	// Fail closed means NOTHING was processed.
	appDB := dbtest.OpenMerchantDB(t, id.UUID())
	var n int
	require.NoError(t, appDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM billing.subscriptions WHERE merchant_id = $1`, id.UUID()).Scan(&n))
	require.Zero(t, n, "no subscription may be created from an unarmed rail's webhook")
}

// fetchCCBillPriceID resolves the seeded catalog's price id over the public
// catalog surface (same as host-two's harness does).
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

// Seed only a historical pending provider reservation. New CCBill recurring
// enrollment is refused; its already-created provider callbacks remain supported.
func seedLegacyCCBillCheckoutSession(t *testing.T, ctx context.Context, id merchant.ID, priceID, userID string) string {
	t.Helper()
	database := dbtest.OpenMerchantDB(t, id.UUID())
	parsedPrice, err := openrails.ParsePriceID(priceID)
	require.NoError(t, err)
	_, err = database.Pool().Exec(ctx, `INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id.UUID(), uuid.MustParse(userID))
	require.NoError(t, err)
	sessionID := uuid.NewString()
	_, err = database.Pool().Exec(ctx, `INSERT INTO billing.checkout_sessions(id,merchant_id,customer_id,price_id,mode,rail,status,amount,currency,expires_at,psp_id)
 SELECT $1,$2,$3,$4,'subscription','ccbill','requires_action',$5,'USD',now()+interval '1 day',id FROM billing.psps WHERE merchant_id=$2 AND rail='ccbill' AND archived=false`, sessionID, id.UUID(), uuid.MustParse(userID), parsedPrice.UUID(), ccbillWebhookTestPriceMicros)
	require.NoError(t, err)
	return sessionID
}
