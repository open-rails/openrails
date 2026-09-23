//go:build integration

package tests

// Explicit real NMI sandbox qualification. This is never fake-provider proof.
// Run only with NMI_SANDBOX_SECURITY_KEY and NMI_WEBHOOK_SIGNING_SECRET set.
// Browser Collect.js tokenization remains a separate qualification: this test
// creates a sandbox vault directly, then uses verified customer HTTP checkout.
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
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

const (
	nmiE2ETestCard   = "4111111111111111"
	nmiE2ECardExpiry = "1228"
	nmiE2ECardCVV    = "123"
)

func TestNMILiveLifecycleE2E(t *testing.T) {
	key, secret := strings.TrimSpace(os.Getenv("NMI_SANDBOX_SECURITY_KEY")), strings.TrimSpace(os.Getenv("NMI_WEBHOOK_SIGNING_SECRET"))
	if key == "" || secret == "" {
		t.Skip("real sandbox NMI_SANDBOX_SECURITY_KEY and NMI_WEBHOOK_SIGNING_SECRET required")
	}
	f := newTreasuryWorkflow(t)
	rt := f.surface.App().Runtime
	integrationharness.SeedPSPs(t.Context(), t, rt, f.merchant.MerchantID, config.PSPSet{"nmi": {Rail: "nmi", AccountID: "qualification-" + uuid.NewString(), NMI: &config.NMIRailConfig{SecurityKey: key, WebhookSigningSecret: secret}}})
	provider, err := nmi.NewAccountClient(uuid.New(), uuid.New(), "nmi", &config.NMIProviderSettings{SecurityKey: key, WebhookSecret: secret}, true)
	require.NoError(t, err)
	require.Equal(t, nmi.DefaultDirectPostURL, provider.DirectPostURL)
	require.Equal(t, nmi.DefaultQueryAPIURL, provider.QueryURL)
	ctx := merchant.WithID(t.Context(), f.merchant.MerchantID)
	// The actual host cancel route queues River work; run the existing fleet to
	// consume that public request, with the test owning its lifetime.
	workerCtx, cancel := context.WithCancel(context.Background())
	require.NoError(t, rt.InitRiver(workerCtx))
	workers := make(chan error, 1)
	go func() { workers <- rt.RunWorkers(workerCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-workers; err != nil {
			require.ErrorIs(t, err, context.Canceled)
		}
	})
	t.Cleanup(func() {
		require.NoError(t, rt.DB.RunInMerchantConn(context.WithoutCancel(ctx), func(ctx context.Context) error {
			_, err := rt.DB.Qx(ctx).Exec(ctx, `UPDATE billing.psps SET archived=true WHERE merchant_id=$1`, f.merchant.MerchantID.UUID())
			return err
		}))
	})
	run := uuid.NewString()
	for _, recurring := range []bool{false, true} {
		customer, token := f.actor(t, []string{permissions.CustomerAll})
		amount := (100 + time.Now().UnixNano()%400) * 10_000
		product, err := f.client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "live-" + uuid.NewString(), DisplayName: "NMI sandbox qualification"})
		require.NoError(t, err)
		request := openrails.PriceCreateParams{ProductID: product.ID, UnitAmount: amount, Currency: "USD", PSPLinks: map[string]map[string]string{"nmi": {"rail": "nmi"}}}
		mode := "one_off"
		if recurring {
			mode = "subscription"
			hours := 720
			request.AccessDurationHours = &hours
			request.AutoRenew = true
			plan := fmt.Sprintf("openrails_e2e_nmi_%d_d30", amount)
			ensureNMISandboxPlan(t, provider, key, plan, amount, 30)
			request.PSPLinks["nmi"]["plan_id"] = plan
		}
		price, err := f.client.Prices.Create(t.Context(), &request)
		require.NoError(t, err)
		vault := createNMISandboxVault(t, provider, key)
		method := uuid.New()
		require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
			q := rt.DB.Qx(ctx)
			psp := dbtest.EnsureTestPSP(ctx, t, q, f.merchant.MerchantID.UUID(), "nmi")
			_, err := q.Exec(ctx, `INSERT INTO billing.payment_methods(id,merchant_id,customer_id,psp_id,rail,custodian,rail_customer_ref,initial_transaction_id,last_four,card_type,expiry_date) VALUES($1,$2,$3,$4,'nmi','psp',$5,'','1111','Visa','12/28')`, method, f.merchant.MerchantID.UUID(), customer.UUID(), psp, vault)
			return err
		}))
		status, raw := requestWorkflowJSON(t, http.MethodPost, f.hostURL+"/v1/me/checkout", token, map[string]any{"price_id": price.ID, "mode": mode, "metadata": map[string]string{"e2e_run_id": run}, "payment": map[string]any{"rail": "nmi", "payment_method_id": openrails.PaymentMethodID(method).String()}})
		require.Equal(t, http.StatusOK, status, string(raw))
		var response struct {
			Status         string `json:"status"`
			SubscriptionID string `json:"subscription_id"`
			Payment        struct {
				TransactionID string `json:"transaction_id"`
			} `json:"payment"`
		}
		require.NoError(t, json.Unmarshal(raw, &response))
		if recurring {
			require.Contains(t, []string{"succeeded", "pending"}, response.Status)
		} else {
			require.Equal(t, "succeeded", response.Status)
		}
		require.NotEmpty(t, response.Payment.TransactionID)
		verifyNMITransaction(t, provider, key, response.Payment.TransactionID, amount)
		var subscription uuid.UUID
		var providerSub string
		require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
			q := rt.DB.Qx(ctx)
			if recurring {
				return q.QueryRow(ctx, `SELECT id,rail_subscription_id FROM billing.subscriptions WHERE merchant_id=$1 AND customer_id=$2`, f.merchant.MerchantID.UUID(), customer.UUID()).Scan(&subscription, &providerSub)
			}
			var count int
			err := q.QueryRow(ctx, `SELECT count(*) FROM billing.payments WHERE merchant_id=$1 AND customer_id=$2 AND transaction_id=$3 AND amount=$4 AND currency='USD' AND status='completed'`, f.merchant.MerchantID.UUID(), customer.UUID(), response.Payment.TransactionID, amount).Scan(&count)
			if err != nil {
				return err
			}
			require.Equal(t, 1, count)
			return nil
		}))
		if recurring {
			require.NotEmpty(t, response.SubscriptionID)
			require.NotEmpty(t, providerSub)
			verifyNMIRecurring(t, provider, key, providerSub)
			require.Eventually(t, func() bool {
				var activeAndPaid bool
				err := rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
					return rt.DB.Qx(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing.subscriptions s JOIN billing.payments p ON p.subscription_id=s.id AND p.merchant_id=s.merchant_id WHERE s.id=$1 AND s.status='active' AND p.transaction_id=$2 AND p.amount=$3 AND p.currency='USD' AND p.status='completed')`, subscription, response.Payment.TransactionID, amount).Scan(&activeAndPaid)
				})
				return err == nil && activeAndPaid
			}, 30*time.Second, 500*time.Millisecond)
			status, raw = requestWorkflowJSON(t, http.MethodPost, f.hostURL+"/v1/me/subscriptions/"+openrails.SubscriptionID(subscription).String()+"/cancel", token, map[string]string{"feedback": "sandbox qualification complete"})
			require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, status, string(raw))
			require.Eventually(t, func() bool {
				var state string
				err := rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
					return rt.DB.Qx(ctx).QueryRow(ctx, `SELECT status FROM billing.subscriptions WHERE id=$1`, subscription).Scan(&state)
				})
				return err == nil && state == "cancelled"
			}, 90*time.Second, 500*time.Millisecond)
		}
	}
}

func ensureNMISandboxPlan(t *testing.T, client *nmi.NMIClient, securityKey, planID string, amount int64, cycleDays int) {
	t.Helper()
	out := postNMIForm(t, client.DirectPostURL, url.Values{
		"security_key":  {securityKey},
		"recurring":     {"add_plan"},
		"plan_id":       {planID},
		"plan_name":     {"OpenRails E2E " + planID},
		"plan_amount":   {microUSDDecimalAmount(amount)},
		"day_frequency": {strconv.Itoa(cycleDays)},
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
	railCustomerRef := out.Get("customer_vault_id")
	require.NotEmpty(t, railCustomerRef, "NMI should return a customer_vault_id")
	t.Cleanup(func() {
		postNMIForm(t, client.DirectPostURL, url.Values{
			"customer_vault":    {"delete_customer"},
			"security_key":      {securityKey},
			"customer_vault_id": {railCustomerRef},
			"test_mode":         {"enabled"},
		})
	})
	return railCustomerRef
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

func microUSDDecimalAmount(micros int64) string {
	cents := micros / 10_000
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}
