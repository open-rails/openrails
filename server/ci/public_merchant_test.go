//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// productsOf decodes a product list answer.
func productsOf(t *testing.T, w *httptest.ResponseRecorder) []billing.Product {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var page billing.ListPage[billing.Product]
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page), w.Body.String())
	return page.Items
}

// No public route reads the catalog: a host's backend reads it with its own
// credential, which names the merchant. A ?merchant= parameter opens nothing:
// the credential's merchant answers, whatever the query names.
func TestCatalogReadsNameNoMerchant(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	srv := f.resourceServer(t)
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	a, b := provision(t, srv, uniqueName("catalog-a")), provision(t, srv, uniqueName("catalog-b"))
	key := "course-" + uuid.NewString()[:8]
	sell := func(mid billing.MerchantID, amount int64) billing.ProductID {
		shop := handle(t, srv, mid)
		product, err := shop.CreateProduct(ctx, billing.CreateProductParams{Key: key, DisplayName: "Course"})
		require.NoError(t, err)
		_, err = shop.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: "once", Currency: "USD", UnitAmount: amount})
		require.NoError(t, err)
		return product.ID
	}
	productA := sell(a, 5_000_000)
	sell(b, 7_000_000)
	// one is the single product an answer lists, with its one price.
	one := func(w *httptest.ResponseRecorder) (billing.ProductID, int64) {
		t.Helper()
		products := productsOf(t, w)
		require.Len(t, products, 1, w.Body.String())
		require.Len(t, products[0].Prices, 1, w.Body.String())
		return products[0].ID, products[0].Prices[0].UnitAmount
	}

	// The public catalog is gone, signed in or out.
	keyA := merchantKey(t, srv, a, "owner")
	public := "/v1/products?" + url.Values{"merchant": {a.String()}, "keys": {key}}.Encode()
	for _, authorization := range []string{"", "Bearer " + keyA.Secret} {
		w := serve(handler, rsRequest{path: public, authorization: authorization})
		require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	}

	// Catalog reads are catalog management's, behind its auth.
	management := "/v1/catalog/products?keys=" + url.QueryEscape(key)
	require.Equal(t, http.StatusUnauthorized, serve(handler, rsRequest{path: management}).Code)
	viewer := merchantKey(t, srv, a, "viewer")
	id, amount := one(serve(handler, rsRequest{path: management, authorization: "Bearer " + viewer.Secret}))
	require.Equal(t, productA, id)
	require.EqualValues(t, 5_000_000, amount)
	write := serve(handler, rsRequest{method: http.MethodPost, path: "/v1/catalog/products", authorization: "Bearer " + viewer.Secret})
	require.Equal(t, http.StatusForbidden, write.Code, write.Body.String())
	id, _ = one(serve(handler, rsRequest{path: management, authorization: "Bearer " + keyA.Secret}))
	require.Equal(t, productA, id)

	// A merchant's backend learns the id its pages name from its access read.
	access := serve(handler, rsRequest{path: "/v1/admin/access", authorization: "Bearer " + keyA.Secret})
	require.Equal(t, http.StatusOK, access.Code, access.Body.String())
	require.Contains(t, access.Body.String(), `"scope_id":"`+a.String()+`"`)

	// Elsewhere the parameter names nothing: A's key reads A's rows, or the
	// route refuses the parameter; it never reads B's.
	for _, path := range []string{"/v1/catalog/products"} {
		w := serve(handler, rsRequest{path: path + "?" + url.Values{"merchant": {b.String()}, "keys": {key}}.Encode(), authorization: "Bearer " + keyA.Secret})
		if w.Code == http.StatusBadRequest {
			continue
		}
		products := productsOf(t, w)
		require.Len(t, products, 1, "%s: %s", path, w.Body.String())
		require.Equal(t, productA, products[0].ID, path)
	}
	// Product reads share one authenticated catalog route.
	retired := serve(handler, rsRequest{path: "/v1/app/catalog/products", authorization: "Bearer " + keyA.Secret})
	require.Equal(t, http.StatusNotFound, retired.Code, retired.Body.String())
}

// GET /v1/config is the deployment's alone: every caller reads the same
// document, which names no merchant. A merchant's payment setup and name
// answer only its own customers, at GET /v1/me/config.
func TestPublicConfigHasNoMerchantPart(t *testing.T) {
	w := newWorld(t)
	martLogin := newIssuerKey(t, "https://mart-"+uuid.NewString()[:8]+".e2e.test")
	trust(t, w.srv, w.mart, martLogin.app(t, "viewer", nil))
	shopper, martShopper := w.customer(w.shopLogin, uuid.NewString(), nil), w.customer(martLogin, uuid.NewString(), nil)

	anonymous := serve(w.handler, rsRequest{path: "/v1/config"})
	require.Equal(t, http.StatusOK, anonymous.Code, anonymous.Body.String())
	require.Equal(t, "public, max-age=300", anonymous.Header().Get("Cache-Control"))
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(anonymous.Body.Bytes(), &doc))
	require.NotContains(t, doc, "payment")
	require.NotContains(t, doc, "merchant")
	for _, key := range []string{"capabilities", "currencies", "rails", "captcha", "network"} {
		require.Contains(t, doc, key)
	}
	for name, q := range map[string]rsRequest{
		"a customer's token": {path: "/v1/config", authorization: shopper},
		"a merchant header":  {path: "/v1/config", header: map[string]string{retiredMerchantHeader: "id:" + w.shop.String()}},
		"a merchant query":   {path: "/v1/config?merchant=" + w.shop.String()},
	} {
		got := serve(w.handler, q)
		require.Equal(t, http.StatusOK, got.Code, "%s: %s", name, got.Body.String())
		require.Equal(t, anonymous.Body.String(), got.Body.String(), name)
		require.Equal(t, anonymous.Header().Get("ETag"), got.Header().Get("ETag"), name)
	}

	// setup is a customer's read of its merchant's part.
	setup := func(q rsRequest) billing.CustomerConfig {
		t.Helper()
		got := serve(w.handler, q)
		require.Equal(t, http.StatusOK, got.Code, got.Body.String())
		require.Equal(t, "private", got.Header().Get("Cache-Control"))
		var cfg billing.CustomerConfig
		require.NoError(t, json.Unmarshal(got.Body.Bytes(), &cfg), got.Body.String())
		require.Len(t, cfg.Payment.PSPs, 1, got.Body.String())
		return cfg
	}
	shop := setup(rsRequest{path: "/v1/me/config", authorization: shopper})
	require.Equal(t, "Network Shop", shop.Merchant.DisplayName)
	require.Equal(t, "test", shop.Payment.PSPs[0].Config["tokenization_key"])
	mart := setup(rsRequest{path: "/v1/me/config", authorization: martShopper})
	require.Equal(t, "Network Mart", mart.Merchant.DisplayName)
	require.Equal(t, "mart", mart.Payment.PSPs[0].Config["tokenization_key"])
	// Naming another merchant changes nothing: the token's merchant answers.
	require.Equal(t, shop, setup(rsRequest{path: "/v1/me/config?merchant=" + w.mart.String(), authorization: shopper}))

	none := serve(w.handler, rsRequest{path: "/v1/me/config"})
	require.Equal(t, http.StatusUnauthorized, none.Code, none.Body.String())
	require.Equal(t, billing.CodeAuthenticationRequired, errorCode(t, none))
}

// On the payment origin a pay URL's secret reads its order's merchant's
// payment setup and name, never stored by a cache; the deployment's
// configuration there names no merchant either.
func TestPurchaseSecretReadsItsMerchantConfig(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	// secret is the secret of a purchase a merchant's backend made.
	secret := func(mid billing.MerchantID) string {
		t.Helper()
		shop := handle(t, w.srv, mid)
		customer := billing.CustomerID(uuid.New())
		_, err := shop.UpdateCustomer(ctx, customer, billing.UpdateCustomerParams{})
		require.NoError(t, err)
		key := "item-" + uuid.NewString()[:8]
		product, err := shop.CreateProduct(ctx, billing.CreateProductParams{Key: key, DisplayName: "Item " + key})
		require.NoError(t, err)
		price, err := shop.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: key, Currency: "USD", UnitAmount: 5_000_000})
		require.NoError(t, err)
		order, err := shop.CreateOrder(ctx, billing.CreateOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: price.ID}},
			PayURL: &billing.OrderPayURLParams{SuccessURL: "https://shop.e2e.test/thanks"}, IdempotencyKey: uuid.NewString()})
		require.NoError(t, err)
		require.NotNil(t, order.PayURLExpiresAt)
		_, s, ok := strings.Cut(*order.PayURL, "#")
		require.True(t, ok)
		return s
	}
	read := func(host, path, bearer string) *httptest.ResponseRecorder {
		q := rsRequest{path: "https://" + host + path}
		if bearer != "" {
			q.authorization = "Bearer " + bearer
		}
		return serve(w.handler, q)
	}
	shop, mart := secret(w.shop), secret(w.mart)

	for _, tc := range []struct{ secret, name, key string }{{shop, "Network Shop", "test"}, {mart, "Network Mart", "mart"}} {
		got := read(paymentHost, "/v1/me/config", tc.secret)
		require.Equal(t, http.StatusOK, got.Code, got.Body.String())
		require.Equal(t, "no-store", got.Header().Get("Cache-Control"))
		var cfg billing.CustomerConfig
		require.NoError(t, json.Unmarshal(got.Body.Bytes(), &cfg), got.Body.String())
		require.Equal(t, tc.name, cfg.Merchant.DisplayName)
		require.Len(t, cfg.Payment.PSPs, 1, got.Body.String())
		require.Equal(t, tc.key, cfg.Payment.PSPs[0].Config["tokenization_key"])
	}

	deployment := read(paymentHost, "/v1/config", shop)
	require.Equal(t, http.StatusOK, deployment.Code, deployment.Body.String())
	require.Equal(t, "no-store", deployment.Header().Get("Cache-Control"))
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(deployment.Body.Bytes(), &doc))
	require.NotContains(t, doc, "payment")
	require.NotContains(t, doc, "merchant")

	refused(t, read(paymentHost, "/v1/me/config", ""), http.StatusNotFound, "pay_url_not_found")
	// The secret is a credential on the payment origin alone.
	require.Equal(t, http.StatusUnauthorized, read(serverHost, "/v1/me/config", shop).Code)
}

// customerOf is a customer token of a merchant's own login.
func customerOf(t *testing.T, login issuerKey, sub string) string {
	return "Bearer " + login.mint(t, func(c jwt.MapClaims) {
		c["sub"], c["scope"] = sub, billing.ScopeSelf
		delete(c, "permissions")
	})
}
