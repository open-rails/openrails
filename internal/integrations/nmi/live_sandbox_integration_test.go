//go:build integration

package nmi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// TestLiveSandboxClientSurface drives every #663-ported client method against
// the REAL NMI sandbox (opt-in via NMI_SANDBOX_SECURITY_KEY; skips otherwise).
// This is the proof that OUR code — not curl — speaks the live v5 dialect:
// plans (v5 create/get + classic edit), vault (v5 update/delete + v5 vault
// sale), payments (v5 auth/void/get + settled-only refund refusal), the
// subscription tombstone read, and the query.php transaction search.
func TestLiveSandboxClientSurface(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("NMI_SANDBOX_SECURITY_KEY"))
	if key == "" {
		t.Skip("NMI_SANDBOX_SECURITY_KEY not set; skipping live sandbox client-surface proof")
	}

	client, err := NewClient("live-sandbox", &config.NMIProviderSettings{SecurityKey: key}, true)
	require.NoError(t, err)
	require.Equal(t, DefaultV5BaseURL, client.V5BaseURL, "must hit the real v5 gateway, not a stub")

	runID := fmt.Sprintf("%d", time.Now().UnixNano())

	// --- test-mode probe: v5 auth + void on the non-issued test card ---
	probe, err := client.ProbeTestMode(context.Background())
	require.NoError(t, err)
	require.Equal(t, ProbeSimulated, probe, "the sandbox account must be simulating")

	// --- plans: v5 create -> v5 get -> classic edit -> v5 get -> cleanup ---
	planID := "openrails-live-surface-" + runID[:10]
	require.NoError(t, client.AddRecurringPlan(context.Background(), planID, "Live Surface Probe", 123, 30, 0))
	detail, err := client.GetRecurringPlanDetailByID(context.Background(), planID)
	require.NoError(t, err)
	require.True(t, detail.Found)
	assert.Equal(t, int64(123), detail.AmountCents)
	assert.Equal(t, 30, detail.DayFrequency)
	require.NoError(t, client.EditRecurringPlan(context.Background(), planID, "Live Surface Probe v2", 321))
	detail, err = client.GetRecurringPlanDetailByID(context.Background(), planID)
	require.NoError(t, err)
	require.True(t, detail.Found)
	assert.Equal(t, int64(321), detail.AmountCents, "classic edit_plan must be visible through the v5 read")
	t.Cleanup(func() {
		_ = client.sendV5Request(context.Background(), http.MethodDelete, "/plans/"+url.PathEscape(planID), nil, nil)
	})

	// --- vault: classic create (raw-PAN shortcut), then OUR v5 paths ---
	// The REAL token path (Collect.js token -> v5 create) is proven separately
	// by TestLiveCollectJSTokenVaultCreate (headless-Chrome tokenization).
	// Production vault creation takes a Collect.js token (browser-only), so the
	// vault is minted with the raw test PAN via classic direct post; everything
	// after is the real client surface.
	form := url.Values{
		"security_key":   {key},
		"customer_vault": {"add_customer"},
		"ccnumber":       {"4111111111111111"},
		"ccexp":          {"1029"},
		"first_name":     {"LiveSurface"},
		"last_name":      {"Probe"},
	}
	resp, err := http.PostForm(client.DirectPostURL, form)
	require.NoError(t, err)
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	_ = resp.Body.Close()
	vals, err := url.ParseQuery(string(body[:n]))
	require.NoError(t, err)
	vaultID := vals.Get("customer_vault_id")
	require.NotEmpty(t, vaultID, "classic add_customer must return a vault id: %s", string(body[:n]))
	t.Cleanup(func() {
		_ = client.DeleteCustomerVault(context.Background(), DeleteCustomerVaultData{CustomerVaultID: vaultID})
	})

	// v5 customer update (lookup billing id -> PATCH).
	require.NoError(t, client.UpdateCustomerVault(context.Background(), UpdateCustomerVaultData{
		CustomerVaultID:         vaultID,
		CreateCustomerVaultData: CreateCustomerVaultData{FirstName: "LiveSurfaceUpdated", City: "Testville"},
	}))

	// v5 vault sale (customer_vault:{id} form), randomized amount to dodge
	// duplicate detection.
	amountCents := moneyutil.Cents(110 + time.Now().UnixNano()%80)
	orderID := "live-surface-" + runID[:12]
	sale, err := client.RunSale(context.Background(), SaleParams{
		CustomerVaultID:  vaultID,
		Amount:           amountCents,
		Currency:         "USD",
		OrderDescription: "live client-surface probe",
		OrderID:          orderID,
	})
	require.NoError(t, err)
	require.NotEmpty(t, sale.TransactionID)

	// v5 single-payment read + actions.
	txn, found, err := client.GetPayment(context.Background(), sale.TransactionID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "1", txn.Response)
	actions, found, err := client.GetPaymentActions(context.Background(), sale.TransactionID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, actions)
	assert.Equal(t, "sale", actions[0].Type)
	assert.Equal(t, int64(amountCents), actions[0].AmountCents)

	// v5 refund: the test-mode simulator refunds an unsettled sale immediately
	// (observed live); a production-posture gateway would refuse (settled-only,
	// same as classic). Accept either — what matters is the route + body shape
	// + error mapping are the live dialect, never a route/shape rejection.
	refundResp, refundErr := client.Refund(context.Background(), RefundParams{TransactionID: sale.TransactionID, Amount: amountCents})
	if refundErr != nil {
		assert.NotContains(t, refundErr.Error(), "Route not found", "refund route must exist")
		assert.NotContains(t, refundErr.Error(), "extraParameters", "refund body shape must be accepted")
		// Unsettled sale still open — void releases it.
		require.NoError(t, client.Void(context.Background(), sale.TransactionID))
	} else {
		require.NotEmpty(t, refundResp.TransactionID, "simulated refund must carry a transaction id")
	}

	// query.php transaction search finds the sale by order reference.
	probeRes, err := client.ProbeSalesByOrderID(context.Background(), orderID, time.Time{})
	require.NoError(t, err)
	assert.True(t, probeRes.SuccessFound, "query.php search must find the v5-created sale by order id")
	assert.Equal(t, sale.TransactionID, probeRes.SuccessTransactionID)

	// Subscription reads: a never-existed id is gone (404 path).
	liveness, err := client.GetRecurringLiveness(context.Background(), "999999999999")
	require.NoError(t, err)
	assert.False(t, liveness.Found)

	// v5 rosters paginate without error.
	_, err = client.ListRecurringPlans(context.Background())
	require.NoError(t, err)
	page, err := client.ListCustomersPage(context.Background(), "", 5, "")
	require.NoError(t, err)
	_ = page
	subs, err := client.ListSubscriptionsPage(context.Background(), "", 5)
	require.NoError(t, err)
	_ = subs

	// --- multi-entry vault (#682 shared-vault support), all through OUR code ---
	// Add a second billing entry (v5 POST /customers/{id}/billing — the LIVE
	// route; the documented /billing-addresses answers E_ROUTE_NOT_FOUND).
	var added struct {
		Billing []struct {
			ID string `json:"id"`
		} `json:"billing"`
		ID string `json:"id"`
	}
	require.NoError(t, client.sendV5Request(context.Background(), http.MethodPost, "/customers/"+url.PathEscape(vaultID)+"/billing",
		map[string]any{"first_name": "LiveSurface", "last_name": "SecondCard", "currency": "USD",
			"payment_details": map[string]any{"card_number": "4111111111111111", "card_exp": "1030"}}, &added))
	page2, err := client.ListCustomersPage(context.Background(), "", 5, vaultID)
	require.NoError(t, err)
	require.Len(t, page2.Customers, 1)
	require.Len(t, page2.Customers[0].Billing, 2, "vault must now hold two billing entries")
	var entry1, entry2 string
	for _, b := range page2.Customers[0].Billing {
		if b.PaymentDetails.CardExp == "1030" {
			entry2 = b.ID
		} else {
			entry1 = b.ID
		}
	}
	require.NotEmpty(t, entry1)
	require.NotEmpty(t, entry2)

	// Billing-TARGETED sale (classic lane) must charge the SECOND card.
	targetAmount := moneyutil.Cents(110 + (time.Now().UnixNano()/13)%80)
	targeted, err := client.RunSale(context.Background(), SaleParams{
		CustomerVaultID:  vaultID,
		BillingID:        entry2,
		Amount:           targetAmount,
		Currency:         "USD",
		OrderDescription: "live multi-entry targeted probe",
		OrderID:          "live-multi-" + runID[:10],
	})
	require.NoError(t, err)
	txn2, found, err := client.GetPayment(context.Background(), targeted.TransactionID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "1", txn2.Response)
	require.NoError(t, client.Void(context.Background(), targeted.TransactionID))

	// Entry-scoped delete removes ONE card; the sibling survives.
	require.NoError(t, client.DeleteCustomerBillingEntry(context.Background(), vaultID, entry2))
	page3, err := client.ListCustomersPage(context.Background(), "", 5, vaultID)
	require.NoError(t, err)
	require.Len(t, page3.Customers[0].Billing, 1, "one entry must remain after the scoped delete")
	assert.Equal(t, entry1, page3.Customers[0].Billing[0].ID)

	// The LAST entry cannot be deleted (NMI refuses to empty a vault) — the
	// whole-vault delete below is the correct final step.
	err = client.DeleteCustomerBillingEntry(context.Background(), vaultID, entry1)
	require.Error(t, err, "deleting the last billing entry must be refused by the gateway")

	// v5 vault delete, then the roster no longer lists the customer.
	require.NoError(t, client.DeleteCustomerVault(context.Background(), DeleteCustomerVaultData{CustomerVaultID: vaultID}))
	gone, err := client.ListCustomersPage(context.Background(), "", 5, vaultID)
	require.NoError(t, err)
	for _, c := range gone.Customers {
		require.NotEqual(t, vaultID, c.ID, "deleted vault must leave the roster")
	}
}
