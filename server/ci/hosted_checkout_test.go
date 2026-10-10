//go:build e2e && integration

package ci_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/hostedcheckout"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/server"
)

const (
	checkoutHost = "checkout.e2e.test"
	apiHost      = "api.e2e.test"
)

var checkoutPage = fstest.MapFS{
	"index.html":    {Data: []byte("<!doctype html><title>Checkout</title>")},
	"assets/app.js": {Data: []byte("void 0")},
}

// The hosted checkout host serves the page and, under one checkout URL's
// secret, that order's own routes as its customer: never another order,
// never saved cards the merchant did not offer, never on the API host, and
// not after the URL expires.
func TestHostedCheckoutSecretOpensOneOrder(t *testing.T) {
	f := newFixture(t)
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Second))
	cp := f.newVaultServer(t, func(cfg *server.Config, deps *server.Deps) {
		cfg.Engine.PublicBillingBaseURL = "https://" + apiHost
		cfg.HostedCheckoutURL = "https://" + checkoutHost
		deps.Engine.Clock = clock
		deps.CheckoutAssets = checkoutPage
	})
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	rt := engine.Graph(cp.Client()).Runtime
	require.Equal(t, "https://"+checkoutHost, rt.HostedCheckoutOrigin)

	entry, err := cp.ProvisionMerchant(t.Context(), billing.ProvisionMerchantParams{Slug: uniqueName("checkout"), DisplayName: "Checkout Shop"})
	require.NoError(t, err)
	mid := entry.MerchantID
	customer := billing.CustomerID(uuid.New())
	// A merchant order with a checkout, as POST /v1/app/orders makes one.
	newOrder := func(key string, saved bool) (billing.OrderID, string) {
		t.Helper()
		product, err := cp.Client().CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key}, openrails.ForMerchantID(mid))
		require.NoError(t, err)
		price, err := cp.Client().CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key, Currency: "USD", UnitAmount: 5_000_000}, openrails.ForMerchantID(mid))
		require.NoError(t, err)
		ctx := merchant.WithID(t.Context(), mid)
		order, err := rt.Orders.Create(ctx, orders.CreateInput{CustomerID: customer.UUID(), Origin: billing.OrderOriginMerchant,
			Lines: []billing.OrderLineParams{{PriceID: price.ID}}, TTL: hostedcheckout.DefaultLifetime})
		require.NoError(t, err)
		id := billing.OrderID(order.ID)
		plan, err := hostedcheckout.Plan(billing.OrderCheckoutParams{SuccessURL: "https://shop.e2e.test/thanks?order={ORDER_ID}", SavedPaymentMethods: saved}, id, clock.Now(), &order.ExpiresAt)
		require.NoError(t, err)
		require.Equal(t, "https://shop.e2e.test/thanks?order="+id.String(), plan.SuccessURL)
		url, err := hostedcheckout.Attach(ctx, rt.DB.Gen(ctx), rt.HostedCheckoutOrigin, mid, plan, clock.Now())
		require.NoError(t, err)
		page, secret, ok := strings.Cut(url, "#")
		require.True(t, ok)
		require.Equal(t, "https://"+checkoutHost+"/c/"+id.String(), page)
		_, err = hostedcheckout.Attach(ctx, rt.DB.Gen(ctx), rt.HostedCheckoutOrigin, mid, plan, clock.Now())
		require.Error(t, err, "an order takes one checkout")
		return id, secret
	}
	orderA, secretA := newOrder("first", false)
	orderB, secretB := newOrder("second", true)

	call := func(host, method, path, secret string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var payload bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&payload).Encode(body))
		}
		req := httptest.NewRequest(method, "https://"+host+path, &payload)
		req.RemoteAddr = "192.0.2.10:1234"
		req.Header.Set("Content-Type", "application/json")
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	refused := func(res *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		require.Equal(t, status, res.Code, res.Body.String())
		require.Contains(t, res.Body.String(), `"`+code+`"`)
	}

	// The page, framed by nothing, leaking no referrer.
	page := call(checkoutHost, http.MethodGet, "/c/"+orderA.String(), "", nil)
	require.Equal(t, http.StatusOK, page.Code)
	require.Contains(t, page.Body.String(), "Checkout")
	require.Contains(t, page.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")
	require.Equal(t, "DENY", page.Header().Get("X-Frame-Options"))
	require.Equal(t, "no-referrer", page.Header().Get("Referrer-Policy"))
	require.Equal(t, "no-store", page.Header().Get("Cache-Control"))
	require.Equal(t, http.StatusOK, call(checkoutHost, http.MethodGet, "/assets/app.js", "", nil).Code)
	require.Equal(t, http.StatusNotFound, call(checkoutHost, http.MethodGet, "/c/not-an-order", "", nil).Code)
	require.Equal(t, http.StatusNotFound, call(checkoutHost, http.MethodGet, "/admin/", "", nil).Code)

	// The secret reads its own order as its customer.
	own := call(checkoutHost, http.MethodGet, "/v1/me/orders/"+orderA.String(), secretA, nil)
	require.Equal(t, http.StatusOK, own.Code, own.Body.String())
	require.Equal(t, "no-store", own.Header().Get("Cache-Control"))
	var read billing.Order
	require.NoError(t, json.Unmarshal(own.Body.Bytes(), &read))
	require.Equal(t, orderA, read.ID)
	require.Equal(t, customer, read.CustomerID)

	config := call(checkoutHost, http.MethodGet, "/v1/config", secretA, nil)
	require.Equal(t, http.StatusOK, config.Code, config.Body.String())
	require.Equal(t, "no-store", config.Header().Get("Cache-Control"))
	var doc billing.PublicConfig
	require.NoError(t, json.Unmarshal(config.Body.Bytes(), &doc))
	require.NotNil(t, doc.Payment, "the secret names its merchant")
	require.NotNil(t, doc.Merchant)
	require.Equal(t, "Checkout Shop", doc.Merchant.DisplayName)

	// Nothing else: not the customer's other order, not another route.
	refused(call(checkoutHost, http.MethodGet, "/v1/me/orders/"+orderB.String(), secretA, nil), http.StatusNotFound, billing.CodeResourceNotFound)
	refused(call(checkoutHost, http.MethodPost, "/v1/me/orders/"+orderB.String()+"/confirm", secretA, nil), http.StatusNotFound, billing.CodeResourceNotFound)
	refused(call(checkoutHost, http.MethodGet, "/v1/me/orders", secretA, nil), http.StatusNotFound, billing.CodeRouteNotFound)
	refused(call(checkoutHost, http.MethodGet, "/v1/me/subscriptions", secretA, nil), http.StatusNotFound, billing.CodeRouteNotFound)
	refused(call(checkoutHost, http.MethodPost, "/v1/me/orders/"+orderA.String()+"/cancel", secretA, nil), http.StatusNotFound, billing.CodeRouteNotFound)
	// Saved cards only where the merchant offered them.
	refused(call(checkoutHost, http.MethodGet, "/v1/me/payment-methods", secretA, nil), http.StatusNotFound, billing.CodeResourceNotFound)
	cards := call(checkoutHost, http.MethodGet, "/v1/me/payment-methods", secretB, nil)
	require.Equal(t, http.StatusOK, cards.Code, cards.Body.String())

	// No secret, a wrong one, or the API host: nothing.
	refused(call(checkoutHost, http.MethodGet, "/v1/me/orders/"+orderA.String(), "", nil), http.StatusNotFound, "checkout_not_found")
	refused(call(checkoutHost, http.MethodGet, "/v1/config", "", nil), http.StatusNotFound, "checkout_not_found")
	wrong, _, err := hostedcheckout.NewSecret()
	require.NoError(t, err)
	refused(call(checkoutHost, http.MethodGet, "/v1/me/orders/"+orderA.String(), wrong, nil), http.StatusNotFound, "checkout_not_found")
	onAPI := call(apiHost, http.MethodGet, "/v1/me/orders/"+orderA.String(), secretA, nil)
	require.Equal(t, http.StatusUnauthorized, onAPI.Code, onAPI.Body.String())

	// Past its expiry the URL stops working; the order expires with it.
	clock.Advance(hostedcheckout.DefaultLifetime + time.Second)
	refused(call(checkoutHost, http.MethodGet, "/v1/me/orders/"+orderA.String(), secretA, nil), http.StatusGone, "checkout_expired")
	n, err := rt.Orders.ExpireDue(merchant.WithID(t.Context(), mid), 10)
	require.NoError(t, err)
	require.Equal(t, 2, n)
}

// The checkout page runs the PSPs' scripts, so it never shares an origin
// with the API, AuthKit or the product's pages.
func TestHostedCheckoutNeedsItsOwnOrigin(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name string
		edit func(*server.Config)
	}{
		{"api", func(cfg *server.Config) { cfg.Engine.PublicBillingBaseURL = "https://" + checkoutHost }},
		{"issuer", func(cfg *server.Config) { cfg.Auth.Issuer = "https://" + checkoutHost + "/auth" }},
		{"frontend", func(cfg *server.Config) { cfg.FrontendBaseURL = "https://" + checkoutHost }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.buildServer(t, func(cfg *server.Config, deps *server.Deps) {
				cfg.HostedCheckoutURL = "https://" + checkoutHost
				deps.CheckoutAssets = checkoutPage
				tc.edit(cfg)
			})
			require.ErrorContains(t, err, "own origin")
		})
	}
	_, err := f.buildServer(t, func(cfg *server.Config, _ *server.Deps) { cfg.HostedCheckoutURL = "https://" + checkoutHost + "/pay" })
	require.ErrorContains(t, err, "no path")
}
