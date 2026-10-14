//go:build e2e && integration

package ci_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/userinfo"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/openrailstest"
)

// One deployment serves a live merchant and its sandbox side by side, each
// armed on its own CCBill account and selling: the live merchant's customers
// pay on CCBill's own FlexForm, the sandbox's on CCBill's sandbox, and each
// sale completes its own merchant's order. No test seam is wired: none may sit
// beside a live PSP.
func TestLiveAndSandboxMerchantsSellSideBySide(t *testing.T) {
	ctx := t.Context()
	const flexID, rbo = "681cb38f-afb9-4665-931f-2b896072178a", "0000007498"
	directory := &openrailstest.UserInfo{}
	bill, err := openrails.New(ctx, openrails.Config{ProviderWriteMode: openrails.ProviderWritesFull, ReturnOrigins: []string{"https://e2e.test"}, Merchants: map[string]openrails.MerchantDeclaration{
		"shop": {DisplayName: "Shop", PSPs: openrails.PSPs{
			"ccbill": openrails.CCBillPSP{ClientAccnum: "900001", ClientSubacc: "0000", Salt: "live-salt"}.PSPConfig()}},
		"shop-sandbox": {DisplayName: "Shop Sandbox", Sandbox: true, PSPs: openrails.PSPs{
			"ccbill": openrails.CCBillPSP{Environment: openrails.EnvironmentSandbox, ClientAccnum: "900002", ClientSubacc: "0000", Salt: "sandbox-salt"}.PSPConfig()}},
	}}, openrails.Deps{Postgres: openPool(t, emptyDatabase(t)), UserInfo: directory})
	require.NoError(t, err)
	t.Cleanup(func() { _ = bill.Close(ctx) })
	require.NoError(t, bill.Start(ctx))
	require.NoError(t, probe(t, bill, "openrails_configuration"), "every PSP agrees with its merchant")
	auth := &authtest.Fake{}
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, bill, openrails.Routes{Auth: auth}))

	for slug, want := range map[string]struct{ form, account string }{
		"shop":         {"https://api.ccbill.com/wap-frontflex/flexforms/" + flexID, "900001-0000"},
		"shop-sandbox": {"https://sandbox-api.ccbill.com/wap-frontflex/flexforms/" + flexID, "900002-0000"},
	} {
		shop, err := bill.MerchantBySlug(slug)
		require.NoError(t, err)
		params, err := catalog.ParseApplicationYAML([]byte(`schema_version: 1
products:
  premium:
    display_name: Premium
    entitlements: ["premium"]
    provider_prices:
      monthly:
        psp: ccbill
        flex_id: ` + flexID + `
        recurring_billing_option_id: "` + rbo + `"
        amount: 23 USD
        billing_interval: 30 days
`))
		require.NoError(t, err)
		_, err = shop.ApplyCatalog(ctx, params, billing.ApplyCatalogParams{})
		require.NoError(t, err, slug)
		prices, err := shop.ListPrices(ctx, billing.PriceListParams{ProductKey: "premium", Key: "monthly"})
		require.NoError(t, err)
		require.Len(t, prices.Items, 1)

		customer := uuid.NewString()
		directory.Put(userinfo.User{ID: customer, Email: slug + "@example.test", Username: "buyer" + customer[:8]})
		order, err := shop.CreateOrder(ctx, billing.CreateOrderParams{CustomerID: cid(customer), IdempotencyKey: uuid.NewString(),
			Lines: []billing.OrderLineParams{{PriceID: prices.Items[0].ID}}})
		require.NoError(t, err)
		token := auth.Issue(authtest.Grant{Identity: authtest.User(customer), Bound: authtest.MerchantScope(shop.MerchantID())})
		call := func(method, path string, body any) (int, map[string]any) {
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			req := httptest.NewRequest(method, "/v1/me/orders/"+order.ID.String()+path, bytes.NewReader(raw))
			req.RemoteAddr = freshAddr()
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Idempotency-Key", uuid.NewString())
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			out := map[string]any{}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			return rec.Code, out
		}
		status, paid := call(http.MethodPost, "/pay", map[string]any{"expected_total": "23000000", "payment": map[string]any{"return_url": "https://e2e.test/return"}})
		require.Equal(t, http.StatusOK, status, "%s: %v", slug, paid)
		action := paid["payment"].(map[string]any)["next_action"].(map[string]any)
		link, err := url.Parse(action["url"].(string))
		require.NoError(t, err)
		require.Equal(t, want.form, link.Scheme+"://"+link.Host+link.Path, "%s sells on its own CCBill", slug)

		accnum, subacc, _ := strings.Cut(want.account, "-")
		form := url.Values{"clientAccnum": {accnum}, "clientSubacc": {subacc}, "timestamp": {time.Now().UTC().Format("2006-01-02 15:04:05")},
			"subscriptionId": {"1" + strings.ReplaceAll(uuid.NewString(), "-", "")[:17]}, "transactionId": {"0" + strings.ReplaceAll(uuid.NewString(), "-", "")[:17]},
			"reservationId": {link.Query().Get("reservationId")}, "email": {link.Query().Get("email")}, "username": {link.Query().Get("username")},
			"flexId": {flexID}, "subscriptionTypeId": {rbo},
			"billedInitialPrice": {"23.00"}, "billedRecurringPrice": {"23.00"}, "billedCurrencyCode": {"840"},
			"subscriptionInitialPrice": {"23.00"}, "subscriptionRecurringPrice": {"23.00"}, "subscriptionCurrencyCode": {"840"},
			"initialPeriod": {"30"}, "recurringPeriod": {"30"}, "rebills": {"99"},
			"nextRenewalDate": {time.Now().UTC().Add(30 * 24 * time.Hour).Format("2006-01-02")},
			"paymentType":     {"CREDIT"}, "cardType": {"VISA"}, "last4": {"1111"}, "expDate": {"1235"}}
		req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/ccbill/"+want.account+"?eventType=NewSaleSuccess&eventGroupType=Subscription", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "64.38.212.17:443"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", slug, rec.Body.String())
		require.Eventually(t, func() bool {
			_, read := call(http.MethodGet, "", nil)
			return read["status"] == string(billing.OrderComplete)
		}, 20*time.Second, 50*time.Millisecond, "%s's sale completes its own order", slug)
	}
}
