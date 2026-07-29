//go:build integration

package money_test

import (
	"context"
	"encoding/json"
	"github.com/open-rails/openrails/internal/railresolve"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

const liveRailOptIn = "OPENRAILS_LIVE_RAIL_TESTS"

func TestLiveStripeInvoiceCollectionAgainstTestAccount(t *testing.T) {
	requireLiveRailTest(t)
	secretKey := liveEnvValue("OPENRAILS_TEST_STRIPE_SECRET_KEY")
	if secretKey == "" {
		t.Skip("Stripe test key missing (OPENRAILS_TEST_STRIPE_SECRET_KEY)")
	}
	if !strings.HasPrefix(secretKey, "sk_test_") && !strings.HasPrefix(secretKey, "rk_test_") {
		t.Fatalf("refusing to run Stripe live invoice test without a Stripe test-mode key")
	}

	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	cleanupInvoiceRows(t, pool, ctx, payer)

	// Arm the charge path the way production arms rails (#699): self-discover
	// the acct_… identity (the manifest resolver's GET /v1/account), seed the
	// key into the merchant-secrets store as the test merchant's stripe rail
	// credential, and resolve it back through the production store resolver.
	// The raw env value is never handed to the service directly.
	msvc := merchantsServiceForTest(t, dbi)
	stripeAccountID := stripeAccountIdentity(t, secretKey)
	seedRailMerchantAccountSecrets(t, dbi, msvc, string(models.RailStripe), stripeAccountID, map[string]string{"secret_key": secretKey})
	storeCreds, err := msvc.LoadStripeCredentials(ctx, dbtest.TestMerchantID)
	require.NoError(t, err)
	require.Equal(t, secretKey, storeCreds.SecretKey, "production store resolution must return the seeded rail credential")

	customerID := stripePost(t, secretKey, "/v1/customers", url.Values{
		"email":                         {"openrails-invoice-test@example.invalid"},
		"metadata[openrails_live_test]": {"invoice_collection"},
	})["id"].(string)
	t.Cleanup(func() {
		_ = stripeDeleteNoFail(secretKey, "/v1/customers/"+url.PathEscape(customerID))
	})
	pmID := "pm_card_visa"
	pmID = stripePost(t, secretKey, "/v1/payment_methods/"+url.PathEscape(pmID)+"/attach", url.Values{
		"customer": {customerID},
	})["id"].(string)

	pm := seedPaymentMethodWithRailCustomerRef(t, pool, ctx, payer, string(models.RailStripe), pmID)
	seedRailCustomer(t, pool, ctx, payer, string(models.RailStripe), customerID)
	_, err = svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 1_000_000))
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "live-stripe-invoice-"+time.Now().UTC().Format("150405.000000000"), 750_000)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)

	// Same construction pattern production uses for store-armed Stripe work
	// (catalog webhook registration): a rail set built from the STORE-resolved
	// key, handed to the same StripeService+StripeCollectionAdapter types
	// build_runtime wires into the arrears charger. ProviderWriteMode is
	// declared full: unset now fails CLOSED to readonly, and this test's whole
	// point is a real test-mode invoice write.
	stripeSvc := &subscriptions.StripeService{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull},
		Rails: railresolve.FixedSet{
			"stripe": {Rail: models.RailStripe, AccountID: stripeAccountID, Stripe: &config.StripeRailConfig{SecretKey: storeCreds.SecretKey}},
		},
	}
	ch := money.NewScopedCharger(dbi, map[string]money.CollectionAdapter{
		string(models.RailStripe): money.NewStripeCollectionAdapter(dbi, stripeSvc),
	})
	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	paid, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Equal(t, int64(750_000), paid.AmountPaid)
	require.NotNil(t, paid.ExternalInvoiceID)
	require.True(t, strings.HasPrefix(*paid.ExternalInvoiceID, "in_"))

	stripeInvoice := stripeGet(t, secretKey, "/v1/invoices/"+url.PathEscape(*paid.ExternalInvoiceID)+"?expand%5B%5D=lines&expand%5B%5D=payment_intent")
	require.Equal(t, "paid", strings.TrimSpace(stripeInvoice["status"].(string)))
	require.Positive(t, jsonInt64(stripeInvoice["amount_paid"]), "Stripe invoice must collect real money")
	require.NotEmpty(t, stripeInvoice["payment_intent"], "Stripe invoice must have a payment intent")
	lines, ok := stripeInvoice["lines"].(map[string]any)
	require.True(t, ok)
	require.Positive(t, jsonInt64(lines["total_count"]), "Stripe invoice must include the OpenRails invoice item")
}

func TestLiveNMIInvoiceCollectionAgainstSandbox(t *testing.T) {
	requireLiveRailTest(t)
	securityKey := liveEnvValue("NMI_SANDBOX_SECURITY_KEY")
	if securityKey == "" {
		t.Skip("NMI sandbox security key missing (NMI_SANDBOX_SECURITY_KEY)")
	}

	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	cleanupInvoiceRows(t, pool, ctx, payer)

	// Store-armed like the Stripe leg: seed the sandbox key as the test
	// merchant's NMI rail credential, then resolve it back through checkout's
	// production seam (active account scope -> scoped secret name -> store Get,
	// merchant_rail_secrets.go). NMI identity is operator-declared (#683), so
	// the account_id is an opaque per-run label rather than self-discovered.
	msvc := merchantsServiceForTest(t, dbi)
	nmiAccountID := "live-invoice-" + uuid.NewString()[:8]
	seedRailMerchantAccountSecrets(t, dbi, msvc, string(models.RailNMI), nmiAccountID, map[string]string{"security_key": securityKey})
	secretName, found, err := msvc.ActivePSPSecretName(ctx, dbtest.TestMerchantID, string(models.RailNMI), config.ExpectedProviderEnvironment(true), "security_key")
	require.NoError(t, err)
	require.True(t, found, "seeded NMI rail account must resolve")
	storedSecret, err := msvc.Secrets().Get(ctx, dbtest.TestMerchantID, secretName)
	require.NoError(t, err)
	require.Equal(t, securityKey, storedSecret.Value, "production store resolution must return the seeded rail credential")

	client, err := nmi.NewClient(string(models.RailNMI), &config.NMIProviderSettings{SecurityKey: storedSecret.Value}, true)
	require.NoError(t, err)
	railCustomerRef := createNMISandboxVault(t, client)
	t.Cleanup(func() {
		_ = client.DeleteCustomerVault(nmi.DeleteCustomerVaultData{CustomerVaultID: railCustomerRef})
	})

	pm := seedPaymentMethodWithRailCustomerRef(t, pool, ctx, payer, string(models.RailNMI), railCustomerRef)
	_, err = svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 2_000_000))
	amount := (int64(110) + time.Now().UnixNano()%80) * 10_000
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "live-nmi-invoice-"+time.Now().UTC().Format("150405.000000000"), amount)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)

	ch := money.NewScopedCharger(dbi, money.NewNMICollectionAdapters(map[string]*nmi.NMIClient{
		string(models.RailNMI): client,
	}))
	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	if n != 1 {
		var failureCode, failureMessage string
		_ = pool.QueryRow(ctx, `
			SELECT COALESCE(failure_code, ''), COALESCE(failure_message, '')
			FROM openrails.invoice_payments
			WHERE invoice_id = $1 AND status = 'failed'
			ORDER BY created_at DESC
			LIMIT 1
		`, inv.ID).Scan(&failureCode, &failureMessage)
		t.Fatalf("expected one NMI invoice collection, got %d; failure_code=%q failure_message=%q", n, failureCode, failureMessage)
	}

	paid, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Equal(t, amount, paid.AmountPaid)
}

func requireLiveRailTest(t *testing.T) {
	t.Helper()
	if strings.TrimSpace(os.Getenv(liveRailOptIn)) != "1" {
		t.Skipf("%s=1 is required for live rail invoice tests", liveRailOptIn)
	}
	if !isTruthy(os.Getenv("TEST_MODE")) && !isTruthy(os.Getenv("OPENRAILS_TEST_MODE")) {
		t.Fatalf("TEST_MODE=sandbox is required for live rail invoice tests")
	}
}

func cleanupInvoiceRows(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payer identity.CustomerID) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "sandbox":
		return true
	default:
		return false
	}
}

func liveEnvValue(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	for _, path := range liveEnvPaths() {
		values := readEnvFile(path)
		for _, key := range keys {
			if value := strings.TrimSpace(values[key]); value != "" {
				return value
			}
		}
	}
	return ""
}

func liveEnvPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return nil
	}
	return []string{
		filepath.Join(home, "openrails", ".env"),
		filepath.Join(home, "cozy", "cozy-art", ".env"),
		filepath.Join(home, "doujins", ".env"),
	}
}

func readEnvFile(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		key := strings.TrimSpace(strings.TrimPrefix(parts[0], "export "))
		value := strings.TrimSpace(parts[1])
		if idx := strings.Index(value, " #"); idx >= 0 {
			value = strings.TrimSpace(value[:idx])
		}
		value = strings.Trim(value, `"'`)
		if key != "" {
			out[key] = value
		}
	}
	return out
}

// stripeAccountIdentity mirrors the manifest resolver's Stripe self-discovery
// (GET /v1/account). A restricted test key (rk_test_) may lack account-read
// permission; that case falls back to a declared opaque label (logged) so the
// secret scoping still works the #683 way. Any other failure is fatal.
func stripeAccountIdentity(t *testing.T, secretKey string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.stripe.com/v1/account", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+secretKey)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	switch {
	case resp.StatusCode == http.StatusOK:
		var out struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal(body, &out))
		require.True(t, strings.HasPrefix(out.ID, "acct_"), "GET /v1/account must return the rail identity")
		return out.ID
	case resp.StatusCode == http.StatusForbidden:
		t.Log("stripe: GET /v1/account denied for this restricted key; declaring an opaque rail identity label instead")
		return "acct_openrails_live_invoice_test"
	default:
		t.Fatalf("Stripe API /v1/account failed with status %d: %s", resp.StatusCode, sanitizedStripeError(body))
		return ""
	}
}

func stripePost(t *testing.T, secretKey, path string, values url.Values) map[string]any {
	t.Helper()
	body := stripePostRaw(t, secretKey, path, values)
	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	require.NotEmpty(t, out["id"])
	return out
}

func stripeGet(t *testing.T, secretKey, path string) map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.stripe.com"+path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+secretKey)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if resp.StatusCode >= 400 {
		t.Fatalf("Stripe API %s failed with status %d: %s", path, resp.StatusCode, sanitizedStripeError(body))
	}
	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	require.NotEmpty(t, out["id"])
	return out
}

func jsonInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

func stripePostRaw(t *testing.T, secretKey, path string, values url.Values) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://api.stripe.com"+path, strings.NewReader(values.Encode()))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Idempotency-Key", "openrails-live-invoice-test-"+time.Now().UTC().Format("20060102150405.000000000"))
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if resp.StatusCode >= 400 {
		t.Fatalf("Stripe API %s failed with status %d: %s", path, resp.StatusCode, sanitizedStripeError(body))
	}
	return body
}

func stripeDeleteNoFail(secretKey, path string) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, "https://api.stripe.com"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func sanitizedStripeError(body []byte) string {
	var out struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "unparseable Stripe error"
	}
	return strings.TrimSpace(out.Error.Code + " " + out.Error.Message)
}

func createNMISandboxVault(t *testing.T, client *nmi.NMIClient) string {
	t.Helper()
	values := url.Values{
		"customer_vault": {"add_customer"},
		"security_key":   {client.SecurityKey},
		"ccnumber":       {"4111111111111111"},
		"ccexp":          {"1028"},
		"cvv":            {"999"},
		"first_name":     {"OpenRails"},
		"last_name":      {"InvoiceTest"},
		"address1":       {"888"},
		"zip":            {"77777"},
		"orderid":        {"openrails-vault-" + time.Now().UTC().Format("20060102150405.000000000")},
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, client.DirectPostURL, strings.NewReader(values.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	output, err := url.ParseQuery(string(body))
	require.NoError(t, err)
	if output.Get("response") != "1" {
		t.Fatalf("NMI vault create failed: response=%s text=%s code=%s", output.Get("response"), output.Get("responsetext"), output.Get("response_code"))
	}
	railCustomerRef := strings.TrimSpace(output.Get("customer_vault_id"))
	require.NotEmpty(t, railCustomerRef)
	return railCustomerRef
}
