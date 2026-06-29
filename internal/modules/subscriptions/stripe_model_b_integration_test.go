//go:build stripe_integration

// Package subscriptions integration test: #268 review gate.
//
// This test talks to REAL Stripe TEST mode to confirm that the Model-B upgrade
// proration implemented in UpdateSubscriptionPrice behaves correctly end to end:
//
//   - the prorated upgrade amount (new_full - old_unused) is collected NOW, and
//   - the billing cycle resets so current_period_end is ~1 interval out at the
//     new price.
//
// It is gated behind the `stripe_integration` build tag and requires
// STRIPE_SECRET_KEY (a sk_test_/rk_test_ key) in the environment. The UPGRADE
// itself is performed through OpenRails' own StripeService.UpdateSubscriptionPrice
// with the exact Model-B params the checkout upgrade branch uses
// (proration_behavior=always_invoice, billing_cycle_anchor=now) — we exercise
// OUR function, not a hand-rolled Stripe call.
//
// Run with:
//
//	STRIPE_SECRET_KEY=sk_test_... go test -tags stripe_integration \
//	  -run TestStripeModelBUpgrade_Integration -v ./internal/modules/subscriptions/
package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

const stripeAPIBase = "https://api.stripe.com"

// stripeForm POSTs an application/x-www-form-urlencoded request to Stripe and
// decodes the JSON response into out. Used ONLY for test fixture setup
// (product/price/customer/subscription) and read-back — the upgrade under test
// goes through StripeService.UpdateSubscriptionPrice.
func stripeForm(t *testing.T, key, method, path string, form url.Values, out any) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(context.Background(), method, stripeAPIBase+path, body)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Less(t, resp.StatusCode, 300, "stripe %s %s failed (%d): %s", method, path, resp.StatusCode, string(raw))
	if out != nil {
		require.NoError(t, json.Unmarshal(raw, out), "decode %s: %s", path, string(raw))
	}
}

func stripeGetJSON(t *testing.T, key, path string, out any) {
	t.Helper()
	stripeForm(t, key, http.MethodGet, path, nil, out)
}

func TestStripeModelBUpgrade_Integration(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY"))
	if key == "" {
		t.Skip("STRIPE_SECRET_KEY not set; skipping Stripe TEST-mode integration test")
	}
	if !strings.Contains(key, "test_") {
		t.Fatalf("refusing to run against a non-test Stripe key (must contain 'test_')")
	}

	const (
		oldAmount = int64(2000) // $20.00/month
		newAmount = int64(5000) // $50.00/month
	)

	// 1) Product + two recurring monthly prices ($20 and $50).
	var product struct {
		ID string `json:"id"`
	}
	stripeForm(t, key, http.MethodPost, "/v1/products", url.Values{
		"name": {"OpenRails #268 Model-B verification " + time.Now().Format(time.RFC3339)},
	}, &product)
	t.Logf("created product %s", product.ID)

	mkPrice := func(amount int64) string {
		var p struct {
			ID string `json:"id"`
		}
		stripeForm(t, key, http.MethodPost, "/v1/prices", url.Values{
			"product":             {product.ID},
			"unit_amount":         {fmt.Sprintf("%d", amount)},
			"currency":            {"usd"},
			"recurring[interval]": {"month"},
		}, &p)
		return p.ID
	}
	oldPriceID := mkPrice(oldAmount)
	newPriceID := mkPrice(newAmount)
	t.Logf("created prices: $%.2f=%s  $%.2f=%s", float64(oldAmount)/100, oldPriceID, float64(newAmount)/100, newPriceID)

	// 2) Customer + attach the pm_card_visa test PaymentMethod as default.
	var customer struct {
		ID string `json:"id"`
	}
	stripeForm(t, key, http.MethodPost, "/v1/customers", url.Values{
		"name": {"OpenRails 268 Test"},
	}, &customer)
	t.Logf("created customer %s", customer.ID)

	// Attaching the pm_card_visa shared test PaymentMethod clones it into a real
	// pm_... id scoped to this customer; capture that id for all later references.
	var attachedPM struct {
		ID string `json:"id"`
	}
	stripeForm(t, key, http.MethodPost, "/v1/payment_methods/pm_card_visa/attach", url.Values{
		"customer": {customer.ID},
	}, &attachedPM)
	require.NotEmpty(t, attachedPM.ID, "attached payment method id missing")
	pmID := attachedPM.ID
	stripeForm(t, key, http.MethodPost, "/v1/customers/"+customer.ID, url.Values{
		"invoice_settings[default_payment_method]": {pmID},
	}, nil)

	// 3) Subscribe to the $20 price. Use default_incomplete + confirm so the
	//    first invoice is actually paid (mirrors a real successful checkout) and
	//    the subscription lands "active" with a concrete current_period_end.
	var sub struct {
		ID               string `json:"id"`
		Status           string `json:"status"`
		CurrentPeriodEnd int64  `json:"current_period_end"`
		Items            struct {
			Data []struct {
				ID    string `json:"id"`
				Price struct {
					ID         string `json:"id"`
					UnitAmount int64  `json:"unit_amount"`
				} `json:"price"`
			} `json:"data"`
		} `json:"items"`
		LatestInvoice struct {
			ID            string `json:"id"`
			AmountPaid    int64  `json:"amount_paid"`
			Total         int64  `json:"total"`
			Status        string `json:"status"`
			PaymentIntent string `json:"payment_intent"`
		} `json:"latest_invoice"`
	}
	stripeForm(t, key, http.MethodPost, "/v1/subscriptions", url.Values{
		"customer":               {customer.ID},
		"items[0][price]":        {oldPriceID},
		"default_payment_method": {pmID},
		"payment_behavior":       {"default_incomplete"},
		"expand[]":               {"latest_invoice"},
	}, &sub)
	require.NotEmpty(t, sub.Items.Data, "subscription has no items")
	itemID := sub.Items.Data[0].ID

	// Pay the initial invoice so the subscription becomes active.
	if sub.LatestInvoice.ID != "" && sub.LatestInvoice.Status != "paid" {
		stripeForm(t, key, http.MethodPost, "/v1/invoices/"+sub.LatestInvoice.ID+"/pay", url.Values{
			"payment_method": {pmID},
		}, nil)
	}

	// Re-read the subscription to capture the active-state period boundaries.
	// Use a struct WITHOUT an expanded latest_invoice — on a plain GET Stripe
	// returns latest_invoice as a string id, which would not unmarshal into the
	// object-typed field above.
	var subRead struct {
		ID               string `json:"id"`
		Status           string `json:"status"`
		CurrentPeriodEnd int64  `json:"current_period_end"`
		Items            struct {
			Data []struct {
				ID    string `json:"id"`
				Price struct {
					ID         string `json:"id"`
					UnitAmount int64  `json:"unit_amount"`
				} `json:"price"`
			} `json:"data"`
		} `json:"items"`
	}
	stripeGetJSON(t, key, "/v1/subscriptions/"+sub.ID, &subRead)
	require.NotEmpty(t, subRead.Items.Data, "subscription read has no items")
	subscribedAmount := subRead.Items.Data[0].Price.UnitAmount
	periodEndBefore := time.Unix(subRead.CurrentPeriodEnd, 0).UTC()
	t.Logf("subscribed: sub=%s status=%s price=$%.2f current_period_end(before)=%s",
		subRead.ID, subRead.Status, float64(subscribedAmount)/100, periodEndBefore.Format(time.RFC3339))
	require.Equal(t, oldAmount, subscribedAmount, "should be subscribed to the $20 price")

	// Let a little wall-clock time pass so there is some "unused" old time to
	// credit and so billing_cycle_anchor=now produces a visibly fresh period.
	time.Sleep(3 * time.Second)

	// 4) UPGRADE through OpenRails' OWN code path. These are exactly the Model-B
	//    params the checkout upgrade branch passes.
	svc := &StripeService{
		Config: &config.Config{},
		Rails: config.ProviderAccountSet{
			"stripe": {Rail: models.RailStripe, SecretKey: key},
		},
	}
	require.NoError(t, svc.UpdateSubscriptionPrice(
		context.Background(),
		sub.ID,
		itemID,
		newPriceID,
		"019e5e09-37e6-7ef7-be77-13a9891a13e0", // internal_price_id (new local price)
		"always_invoice",                       // proration_behavior
		"now",                                  // billing_cycle_anchor
	), "UpdateSubscriptionPrice (Model-B upgrade) failed")

	// 5) Fetch the resulting subscription + the upgrade invoice and report.
	var after struct {
		ID               string `json:"id"`
		Status           string `json:"status"`
		CurrentPeriodEnd int64  `json:"current_period_end"`
		Items            struct {
			Data []struct {
				Price struct {
					ID         string `json:"id"`
					UnitAmount int64  `json:"unit_amount"`
				} `json:"price"`
			} `json:"data"`
		} `json:"items"`
	}
	stripeGetJSON(t, key, "/v1/subscriptions/"+sub.ID, &after)
	periodEndAfter := time.Unix(after.CurrentPeriodEnd, 0).UTC()

	// The always_invoice upgrade creates a brand-new invoice for the proration.
	// Grab the most recent invoice for the customer (newest first).
	var invList struct {
		Data []struct {
			ID         string `json:"id"`
			Total      int64  `json:"total"`
			AmountDue  int64  `json:"amount_due"`
			AmountPaid int64  `json:"amount_paid"`
			Status     string `json:"status"`
			Created    int64  `json:"created"`
			Lines      struct {
				Data []struct {
					Amount      int64  `json:"amount"`
					Description string `json:"description"`
				} `json:"data"`
			} `json:"lines"`
		} `json:"data"`
	}
	stripeGetJSON(t, key, "/v1/invoices?customer="+customer.ID+"&limit=10", &invList)
	require.NotEmpty(t, invList.Data, "no invoices found for customer")
	upgradeInv := invList.Data[0] // newest

	// Expected Model-B charge: new_full - old_unused. With the upgrade happening
	// only seconds into the period, old_unused ~= oldAmount and new prorated
	// usage ~= newAmount, so the collected amount should be ~ (newAmount - oldAmount).
	expectedApprox := newAmount - oldAmount

	t.Logf("================ #268 Model-B RESULTS ================")
	t.Logf("subscribed amount:          $%.2f/mo  (price %s)", float64(subscribedAmount)/100, oldPriceID)
	t.Logf("upgraded to:                $%.2f/mo  (price %s)", float64(newAmount)/100, newPriceID)
	t.Logf("subscription price now:     $%.2f/mo  (price %s, status %s)",
		float64(after.Items.Data[0].Price.UnitAmount)/100, after.Items.Data[0].Price.ID, after.Status)
	t.Logf("upgrade invoice:            %s status=%s total=$%.2f amount_due=$%.2f amount_paid=$%.2f",
		upgradeInv.ID, upgradeInv.Status,
		float64(upgradeInv.Total)/100, float64(upgradeInv.AmountDue)/100, float64(upgradeInv.AmountPaid)/100)
	for _, l := range upgradeInv.Lines.Data {
		t.Logf("  line: $%+.2f  %s", float64(l.Amount)/100, l.Description)
	}
	t.Logf("expected ~ (new_full - old_unused) = $%.2f", float64(expectedApprox)/100)
	t.Logf("current_period_end before:  %s", periodEndBefore.Format(time.RFC3339))
	t.Logf("current_period_end after:   %s  (+%s)",
		periodEndAfter.Format(time.RFC3339), periodEndAfter.Sub(time.Now().UTC()).Truncate(time.Minute))
	t.Logf("=====================================================")

	// --- Assertions ---

	// (a) The subscription now bills at the new price.
	require.Equal(t, newPriceID, after.Items.Data[0].Price.ID, "subscription should now be on the new price")
	require.Equal(t, newAmount, after.Items.Data[0].Price.UnitAmount)

	// (b) An upgrade charge was collected NOW (positive, non-trivial).
	require.Equal(t, "paid", upgradeInv.Status, "the upgrade invoice should be paid immediately (always_invoice)")
	require.Positive(t, upgradeInv.Total, "the upgrade invoice should charge a positive amount now")

	// (c) The charged amount is approximately new_full - old_unused. Allow a
	//     wide tolerance because Stripe prorates to the second, but Model-B's
	//     defining property is that it is FAR less than a full new period
	//     (proof old time was credited) and clearly positive (proof new full
	//     period was charged). We assert it lands near (new-old).
	require.InDelta(t, expectedApprox, upgradeInv.Total, float64(oldAmount),
		"collected amount should be ~ (new_full - old_unused) = $%.2f", float64(expectedApprox)/100)
	require.Less(t, upgradeInv.Total, newAmount,
		"Model-B collects a PRORATED upgrade, not a full new period")

	// (d) The cycle RESET: current_period_end is ~1 month out from now.
	dt := periodEndAfter.Sub(time.Now().UTC())
	require.Greater(t, dt, 27*24*time.Hour, "period end should be ~1 month out (cycle reset)")
	require.Less(t, dt, 32*24*time.Hour, "period end should be ~1 month out (cycle reset)")
	require.True(t, periodEndAfter.After(periodEndBefore.Add(-time.Hour)),
		"new period end should not be earlier than the pre-upgrade one")

	// Cleanup: cancel the subscription so it stops openrails. Test-mode objects
	// (product/prices/customer) are harmless and left for inspection.
	stripeForm(t, key, http.MethodDelete, "/v1/subscriptions/"+sub.ID, nil, nil)
	t.Logf("cleaned up: canceled subscription %s (test-mode product/prices/customer left for inspection)", sub.ID)
}
