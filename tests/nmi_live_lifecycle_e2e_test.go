//go:build integration

package tests

// Live NMI lifecycle E2E — the Go replacement for the old
// scripts/mobius_live_lifecycle_e2e.sh shell script. It drives the real
// OpenRails HTTP API surface against a real NMI *sandbox* account (creds from
// the environment) and verifies remote state through NMI's Query API:
//
//	register a live "nmi" provider -> ensure an NMI sandbox recurring plan ->
//	seed catalog (one-off + recurring prices on the "nmi" provider) ->
//	vault a sandbox card -> one-off checkout -> subscription checkout ->
//	verify both at NMI (query.php) -> signed merchant webhook (idempotent x2)
//	-> subscription active -> cancel.
//
// The ONE step that cannot run in-process is browser Collect.js tokenization
// (OpenRails never accepts a raw PAN, #547 — POST /v1/me/payment-methods
// requires an opaque, origin-restricted Collect.js token). Its exact
// server-side equivalent is the NMI Customer Vault (security-key based), so the
// payment method is vaulted directly at NMI here and recorded in OpenRails with
// that real vault id; every subsequent step is the real HTTP API + real NMI.
//
// Requires a real NMI sandbox account: NMI_SANDBOX_SECURITY_KEY and
// NMI_WEBHOOK_SIGNING_SECRET. Skips otherwise.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchants"
)

const (
	nmiE2EProvider   = "nmi"
	nmiE2ETestCard   = "4111111111111111"
	nmiE2ECardExpiry = "1228" // MMYY
	nmiE2ECardCVV    = "123"
)

// TestNMILiveLifecycleE2E exercises the full card lifecycle through the OpenRails
// HTTP API against a real NMI sandbox account.
func TestNMILiveLifecycleE2E(t *testing.T) {
	securityKey := strings.TrimSpace(os.Getenv("NMI_SANDBOX_SECURITY_KEY"))
	if securityKey == "" {
		t.Skip("NMI_SANDBOX_SECURITY_KEY with a real NMI sandbox account key is required for the live lifecycle E2E")
	}

	suite := getSharedTestSuite(t)
	runID := uuid.NewString()

	// 1. Register a LIVE "nmi" provider backed by the real sandbox account and
	// rewire the runtime services to use it (mirrors configureSecondaryNMIProvider
	// but with the real NMI endpoints + sandbox key, not a mock).
	client := registerLiveNMIProvider(t, suite, securityKey)

	// 2. Pick a USD recurring price and a USD one-off price, and bind them to the
	// "nmi" provider. Ensure a matching NMI sandbox recurring plan exists.
	recurring, oneOff := pickUSDPrices(t, suite)
	// NMI flags an identical amount+card as a duplicate transaction, so give each
	// run a unique charge amount (the old shell harness did the same).
	nowUnixNano := time.Now().UnixNano()
	setPriceAmount(t, suite, oneOff, (100+(nowUnixNano%400))*10_000)
	setPriceAmount(t, suite, recurring, (100+((nowUnixNano/7)%400))*10_000)
	planID := fmt.Sprintf("openrails_e2e_nmi_%d_d%d", recurring.Amount, derefInt(recurring.RecurringCycleDays()))
	ensureNMISandboxPlan(t, client, securityKey, planID, recurring)
	bindPriceToNMIProvider(t, suite, recurring.ID, planID)
	bindPriceToNMIProvider(t, suite, oneOff.ID, "")

	// 4. Vault a sandbox card directly at NMI (the browser-Collect.js-equivalent
	// server-side step) and record the OpenRails payment method with the real
	// vault id.
	// Isolate the one-off and subscription flows under separate customers so the
	// one-off entitlement grant doesn't conflict with the subscription purchase.
	// Each gets its own NMI sandbox vault (vault_id is unique per payment method).
	mkVaultedPM := func(user string) *models.PaymentMethod {
		return suite.CreateTestPaymentMethodWithOptions(PaymentMethodOptions{
			UserID:     user,
			Rail:       models.Rail(nmiE2EProvider),
			VaultID:    createNMISandboxVault(t, client, securityKey),
			LastFour:   nmiE2ETestCard[len(nmiE2ETestCard)-4:],
			CardType:   "Visa",
			ExpiryDate: "12/28",
		})
	}
	oneOffUser := uuid.NewString()
	subUser := uuid.NewString()
	oneOffPM := mkVaultedPM(oneOffUser)
	subPM := mkVaultedPM(subUser)
	t.Logf("vaulted NMI sandbox cards: one_off_pm=%s(vault=%s) sub_pm=%s(vault=%s)", oneOffPM.ID, oneOffPM.RailCustomerRef, subPM.ID, subPM.RailCustomerRef)

	oneOffRouter := newHostSeamSelfRouter(t, suite, oneOffUser, nil)
	subRouter := newHostSeamSelfRouter(t, suite, subUser, nil)

	// 5. One-off checkout charging the saved vault, through the HTTP API.
	oneOffResp := postSelfCheckout(t, oneOffRouter, map[string]any{
		"price_id": oneOff.ID.String(),
		"mode":     "one_off",
		"metadata": map[string]string{"e2e_run_id": runID},
		"payment":  map[string]any{"rail": nmiE2EProvider, "payment_method_id": oneOffPM.ID.String()},
	})
	require.Equal(t, "succeeded", oneOffResp.Status, "one-off checkout should be immediately approved by NMI: %+v", oneOffResp)
	require.NotEmpty(t, oneOffResp.Payment.TransactionID, "one-off should carry an NMI transaction id")
	t.Logf("one-off charge approved: nmi_txn=%s", oneOffResp.Payment.TransactionID)

	// DB: a completed payment exists for the one-off.
	assertCompletedPayment(t, suite, oneOffUser, oneOff.Amount, oneOff.Currency)

	// 6. Subscription checkout with the same vault (separate customer).
	subResp := postSelfCheckout(t, subRouter, map[string]any{
		"price_id": recurring.ID.String(),
		"mode":     "subscription",
		"metadata": map[string]string{"e2e_run_id": runID},
		"payment":  map[string]any{"rail": nmiE2EProvider, "payment_method_id": subPM.ID.String()},
	})
	require.Contains(t, []string{"succeeded", "pending"}, subResp.Status, "subscription checkout status: %+v", subResp)
	require.NotEmpty(t, subResp.SubscriptionID, "subscription checkout should return a subscription id")
	require.NotEmpty(t, subResp.Payment.TransactionID, "subscription checkout should carry an NMI transaction id")

	subs := suite.GetAllSubscriptionsByUserID(subUser)
	require.NotEmpty(t, subs, "expected a local subscription row")
	sub := subs[0]
	providerSubID := sub.RailSubscriptionID
	require.NotEmpty(t, providerSubID, "expected the local subscription to carry the NMI recurring id")
	t.Logf("subscription created: local=%s provider=%s nmi_txn=%s", sub.ID, providerSubID, subResp.Payment.TransactionID)

	// 7. Verify remote state at NMI via the Query API.
	verifyNMITransaction(t, client, securityKey, oneOffResp.Payment.TransactionID, oneOff.Amount)
	verifyNMITransaction(t, client, securityKey, subResp.Payment.TransactionID, recurring.Amount)
	verifyNMIRecurring(t, client, securityKey, providerSubID)

	// 8. The immediate NMI approval should have activated the subscription
	// synchronously (#330) — no webhook required for the happy path. (Inbound
	// merchant-webhook signature verification is covered separately by
	// internal/http/routes_merchant_webhook_integration_test.go.)
	requireSubscriptionStatus(t, suite, sub.ID, models.StatusActive, 30*time.Second)

	// 9. Cancel through the HTTP API. Cancellation is async (enqueues a River job
	// that calls NMI and flips the local status), so poll for the terminal state.
	wc := doHostSeamSelf(subRouter, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", `{"feedback":"nmi live e2e cancel"}`)
	require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, wc.Code, "cancel should be accepted: %s", wc.Body.String())
	requireSubscriptionStatus(t, suite, sub.ID, models.StatusCancelled, 90*time.Second)
	t.Logf("== PASS live NMI lifecycle E2E (run %s) ==", runID)
}

// --- checkout response decoding ---

type selfCheckoutResponse struct {
	Status         string `json:"status"`
	SubscriptionID string `json:"subscription_id"`
	Payment        struct {
		TransactionID string `json:"transaction_id"`
	} `json:"payment"`
}

func postSelfCheckout(t *testing.T, router http.Handler, body map[string]any) selfCheckoutResponse {
	t.Helper()
	w := doHostSeamSelf(router, http.MethodPost, "/v1/me/checkout", string(mustJSON(t, body)))
	require.Equalf(t, http.StatusOK, w.Code, "checkout should return 200: %s", w.Body.String())
	var resp selfCheckoutResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return resp
}

// --- live NMI provider wiring (mirror of configureSecondaryNMIProvider, real account) ---

func registerLiveNMIProvider(t *testing.T, suite *TestContainerSuite, securityKey string) *nmi.NMIClient {
	t.Helper()
	suite.Rails[nmiE2EProvider] = &config.RailMerchantAccountConfig{Rail: models.RailNMI, NMI: &config.NMIRailConfig{SecurityKey: securityKey}}

	client, err := nmi.NewClient(nmiE2EProvider, &config.NMIProviderSettings{
		SecurityKey: securityKey,
	}, true)
	require.NoError(t, err)
	// Real NMI sandbox endpoints (defaults) — NOT a mock.
	require.Equal(t, nmi.DefaultDirectPostURL, client.DirectPostURL)
	require.Equal(t, nmi.DefaultQueryAPIURL, client.QueryURL)

	rt := suite.App.Runtime

	// The checkout money path resolves the NMI client from MERCHANT SECRETS
	// first (production posture); the suite seeds a placeholder key there, so
	// overwrite it with the real sandbox key or every charge 401s at NMI.
	secretName, err := merchants.RailMerchantAccountSecretName("nmi", config.ExpectedProviderEnvironment(suite.Config.IsTestMode()), testNMIRailMerchantAccountID, "security_key")
	require.NoError(t, err)
	_, err = rt.Merchants.PutCredential(dbtest.WithTestMerchant(context.Background()), dbtest.TestMerchantID, secretName, securityKey)
	require.NoError(t, err)

	rt.NMIClients[nmiE2EProvider] = client
	if rt.SubscriptionService != nil {
		rt.SubscriptionService.NMIClients = rt.NMIClients
	}
	if rt.VaultService != nil {
		rt.VaultService.NMIClients = rt.NMIClients
	}
	if rt.CheckoutService != nil {
		rt.CheckoutService.NMIClients = rt.NMIClients
		if rt.CheckoutService.NMISaleService != nil {
			rt.CheckoutService.NMISaleService.NMIClients = rt.NMIClients
		}
	}
	return client
}

func bindPriceToNMIProvider(t *testing.T, suite *TestContainerSuite, priceID uuid.UUID, planID string) {
	t.Helper()
	price := suite.GetPrice(priceID)
	if price.Rails == nil {
		price.Rails = map[string]map[string]string{}
	}
	entry := map[string]string{}
	if planID != "" {
		entry[models.RailKeyPlanID] = planID
	}
	price.Rails[nmiE2EProvider] = entry
	railsJSON, err := json.Marshal(price.Rails)
	require.NoError(t, err)
	_, err = suite.Pool.Exec(context.Background(),
		"UPDATE openrails.prices SET rails = $1 WHERE id = $2", railsJSON, price.ID)
	require.NoError(t, err)
}

func setPriceAmount(t *testing.T, suite *TestContainerSuite, p *models.Price, amount int64) {
	t.Helper()
	_, err := suite.Pool.Exec(context.Background(), "UPDATE openrails.prices SET amount = $1 WHERE id = $2", amount, p.ID)
	require.NoError(t, err)
	p.Amount = amount
}

func pickUSDPrices(t *testing.T, suite *TestContainerSuite) (recurring, oneOff *models.Price) {
	t.Helper()
	for _, product := range suite.SeedProducts() {
		for _, p := range product.Prices {
			if !strings.EqualFold(p.Currency, "USD") {
				continue
			}
			if p.RecurringCycleDays() != nil && recurring == nil {
				recurring = p
			}
			if p.RecurringCycleDays() == nil && oneOff == nil {
				oneOff = p
			}
		}
	}
	require.NotNil(t, recurring, "seed data should include a USD recurring price")
	require.NotNil(t, oneOff, "seed data should include a USD one-off price")
	return recurring, oneOff
}

// --- direct NMI sandbox calls (plan, vault, query) ---

func ensureNMISandboxPlan(t *testing.T, client *nmi.NMIClient, securityKey, planID string, recurring *models.Price) {
	t.Helper()
	out := postNMIForm(t, client.DirectPostURL, url.Values{
		"security_key":  {securityKey},
		"recurring":     {"add_plan"},
		"plan_id":       {planID},
		"plan_name":     {"OpenRails E2E " + planID},
		"plan_amount":   {microUSDDecimalAmount(recurring.Amount)},
		"day_frequency": {strconv.Itoa(derefInt(recurring.RecurringCycleDays()))},
		"plan_payments": {"0"},
	})
	// add_plan returns response=1 on create; an already-existing plan_id (NMI
	// has no delete-plan API) is also acceptable and idempotent.
	text := strings.ToLower(out.Get("responsetext"))
	planExists := strings.Contains(text, "already") || strings.Contains(text, "exist") || strings.Contains(text, "duplicate")
	if out.Get("response") != "1" && !planExists {
		t.Fatalf("NMI add_plan failed: response=%s text=%q", out.Get("response"), out.Get("responsetext"))
	}
}

func createNMISandboxVault(t *testing.T, client *nmi.NMIClient, securityKey string) string {
	t.Helper()
	out := postNMIForm(t, client.DirectPostURL, url.Values{
		"customer_vault": {"add_customer"},
		"security_key":   {securityKey},
		"ccnumber":       {nmiE2ETestCard},
		"ccexp":          {nmiE2ECardExpiry},
		"cvv":            {nmiE2ECardCVV},
		"first_name":     {"OpenRails"},
		"last_name":      {"NMIE2E"},
		"address1":       {"888 Test St"},
		"city":           {"Testville"},
		"state":          {"CA"},
		"zip":            {"77777"},
		"country":        {"US"},
		"email":          {"nmi-live-e2e@example.com"},
		"test_mode":      {"enabled"},
	})
	require.Equalf(t, "1", out.Get("response"), "NMI add_customer should succeed: %s", out.Get("responsetext"))
	vaultID := out.Get("customer_vault_id")
	require.NotEmpty(t, vaultID, "NMI should return a customer_vault_id")
	t.Cleanup(func() {
		postNMIForm(t, client.DirectPostURL, url.Values{
			"customer_vault":    {"delete_customer"},
			"security_key":      {securityKey},
			"customer_vault_id": {vaultID},
			"test_mode":         {"enabled"},
		})
	})
	return vaultID
}

func verifyNMITransaction(t *testing.T, client *nmi.NMIClient, securityKey, txnID string, amountMicroUSD int64) {
	t.Helper()
	body := postNMIQuery(t, client.QueryURL, url.Values{
		"security_key":   {securityKey},
		"report_type":    {"transaction"},
		"transaction_id": {txnID},
	})
	require.Containsf(t, body, txnID, "NMI query should report transaction %s", txnID)
	want := microUSDDecimalAmount(amountMicroUSD)
	require.Containsf(t, body, want, "NMI transaction %s should show amount %s", txnID, want)
}

func verifyNMIRecurring(t *testing.T, client *nmi.NMIClient, securityKey, recurringID string) {
	t.Helper()
	body := postNMIQuery(t, client.QueryURL, url.Values{
		"security_key": {securityKey},
		"report_type":  {"recurring"},
		"recurring_id": {recurringID},
	})
	require.Containsf(t, body, recurringID, "NMI query should report recurring subscription %s", recurringID)
}

func postNMIForm(t *testing.T, urlStr string, values url.Values) url.Values {
	t.Helper()
	resp, err := http.PostForm(urlStr, values)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out, err := url.ParseQuery(string(raw))
	require.NoError(t, err)
	return out
}

func postNMIQuery(t *testing.T, urlStr string, values url.Values) string {
	t.Helper()
	resp, err := http.PostForm(urlStr, values)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return string(raw)
}

// --- assertions / helpers ---

func assertCompletedPayment(t *testing.T, suite *TestContainerSuite, userID string, amount int64, currency string) {
	t.Helper()
	for _, p := range suite.GetPaymentsByUserID(userID) {
		if p.Status == "completed" && p.Amount == amount && strings.EqualFold(p.Currency, currency) {
			return
		}
	}
	t.Fatalf("expected a completed payment of %d %s for user %s", amount, currency, userID)
}

func requireSubscriptionStatus(t *testing.T, suite *TestContainerSuite, subID uuid.UUID, want models.SubscriptionStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last models.SubscriptionStatus
	for time.Now().Before(deadline) {
		s := suite.GetSubscription(subID)
		if s != nil {
			last = s.Status
			if s.Status == want {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("subscription %s did not reach status %q within %s (last=%q)", subID, want, timeout, last)
}

func microUSDDecimalAmount(micros int64) string {
	cents := micros / 10_000
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
