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
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/hostedcheckout"
	"github.com/open-rails/openrails/openrailstest/nmimock"
)

func decode[T any](t *testing.T, res *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out), res.Body.String())
	return out
}

// A host's customer buys through billing-ui alone, in the shared-payment-origin
// topology: one app mounts the customer routes and issues checkout URLs on the
// payment origin another app serves, over one database. The signed-in person
// creates its own order on /v1/me with a checkout and gets the URL in the same
// answer; the checkout host resolves it, frames it by the merchant's origins
// and takes the payment. Only the merchant's own person in a session of their
// own asks for one; the success URL is the merchant's; the order is
// idempotent while its URL is a fresh credential on every answer, ending the
// last, and a merchant's own checkout is never touched.
func TestCustomerOrderWithEmbeddedCheckout(t *testing.T) {
	f := newFixture(t)
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Microsecond))
	gateway := nmimock.New(nmimock.Options{Clock: clock.Now})
	t.Cleanup(gateway.Close)
	origins := []string{"https://shop.e2e.test", "https://www.shop.e2e.test"}
	cfg := f.config()
	cfg.ProviderWriteMode = openrails.ProviderWritesFull
	cfg.PublicBillingBaseURL = "https://api.e2e.test"
	cfg.ProviderSandbox = &openrails.ProviderSandboxConfig{NMIGatewayURL: gateway.URL()}
	cfg.RateLimitsDisabled = true
	cfg.Merchants = map[string]openrails.MerchantDeclaration{"shared-checkout": {
		DisplayName: "Shared checkout",
		Settings:    billing.MerchantSettings{Checkout: &billing.MerchantCheckoutSettings{EmbedOrigins: origins}},
		PSPs:        map[string]openrails.PSPConfig{"nmi": nmiPSP()},
	}}
	deps := openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool, Clock: clock}
	publisher, shop := f.boot(t, cfg, deps)
	_, none, err := publisher.HostedCheckout("https://pay.e2e.test", nil)
	require.NoError(t, err)
	require.Nil(t, none, "the publishing app issues URLs and serves no page")
	serving, _ := f.boot(t, cfg, deps)
	_, checkoutHost, err := serving.HostedCheckout("https://pay.e2e.test", fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>Checkout</title>")},
		"assets/app.js": {Data: []byte("void 0")},
	})
	require.NoError(t, err)

	auth := &authtest.Fake{}
	app := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(app, publisher, openrails.Routes{Auth: auth}))
	sell := func(key string, amount int64) billing.PriceID {
		t.Helper()
		product, err := shop.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key})
		require.NoError(t, err)
		price, err := shop.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "once", Currency: "USD", UnitAmount: amount})
		require.NoError(t, err)
		return price.ID
	}
	course, bundle := sell("course", 5_000_000), sell("bundle", 9_000_000)

	call := func(h http.Handler, method, target, bearer string, body any, header map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		var payload bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&payload).Encode(body))
		}
		req := httptest.NewRequest(method, target, &payload)
		req.RemoteAddr = "192.0.2.20:1234"
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		return res
	}
	refusedWith := func(res *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		require.Equal(t, status, res.Code, res.Body.String())
		require.Contains(t, res.Body.String(), `"`+code+`"`)
	}
	embedded := map[string]any{"ui_mode": "embedded"}
	create := func(token, key string, price billing.PriceID, checkout any, header map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		body := map[string]any{"lines": []map[string]any{{"price_id": price}}}
		if checkout != nil {
			body["checkout"] = checkout
		}
		// A browser sends its page's Origin; "" sends none.
		h := map[string]string{"Idempotency-Key": key, "Origin": origins[0]}
		for k, v := range header {
			h[k] = v
		}
		if h["Origin"] == "" {
			delete(h, "Origin")
		}
		return call(app, http.MethodPost, "/v1/me/orders", token, body, h)
	}
	// withCheckout is the answer's order and its checkout's secret.
	withCheckout := func(res *httptest.ResponseRecorder, status int) (billing.Order, string) {
		t.Helper()
		require.Equal(t, status, res.Code, res.Body.String())
		require.Equal(t, "no-store", res.Header().Get("Cache-Control"))
		order := decode[billing.Order](t, res)
		require.Equal(t, billing.OrderOriginCustomer, order.Origin)
		require.NotNil(t, order.Checkout)
		require.NotNil(t, order.Checkout.URL)
		page, secret, ok := strings.Cut(*order.Checkout.URL, "#")
		require.True(t, ok)
		require.Equal(t, "https://pay.e2e.test/c/"+order.ID.String(), page)
		require.True(t, strings.HasPrefix(secret, hostedcheckout.SecretPrefix))
		return order, secret
	}
	onHost := func(method, path, secret string, body any, header map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		return call(checkoutHost, method, "https://pay.e2e.test"+path, secret, body, header)
	}
	opens := func(secret string, id billing.OrderID) billing.Order {
		t.Helper()
		res := onHost(http.MethodGet, "/v1/me/orders/"+id.String(), secret, nil, nil)
		require.Equal(t, http.StatusOK, res.Code, res.Body.String())
		return decode[billing.Order](t, res)
	}

	alice, bob := uuid.NewString(), uuid.NewString()
	aliceToken, bobToken := auth.Person(alice), auth.Person(bob)

	// The browser never reads the catalog.
	res := call(app, http.MethodGet, "/v1/products?"+url.Values{"merchant": {shop.MerchantID().String()}, "keys": {"course"}}.Encode(), "", nil, nil)
	require.Equal(t, http.StatusNotFound, res.Code, res.Body.String())

	// Only the merchant's own person, signed in, asks for a checkout.
	as := func(edit func(*billingauth.Identity), bound billingauth.Scope) string {
		id := authtest.User(alice)
		edit(&id)
		return auth.Issue(authtest.Grant{Identity: id, Bound: bound})
	}
	keep := func(*billingauth.Identity) {}
	for name, token := range map[string]string{
		"an application": auth.Machine(uuid.NewString()),
		"a delegated invoker": as(func(id *billingauth.Identity) {
			id.Invoker = billingauth.Invoker{Issuer: authtest.Issuer, ID: "agent-" + uuid.NewString()[:8]}
		}, billingauth.Scope{}),
	} {
		res := create(token, uuid.NewString(), course, embedded, nil)
		require.Equal(t, http.StatusForbidden, res.Code, "%s: %s", name, res.Body.String())
		require.Contains(t, res.Body.String(), `"`+billing.CodeInvokerScopedPrincipal+`"`, name)
	}
	for name, token := range map[string]string{
		"an API key":      as(func(id *billingauth.Identity) { id.Credential.Kind = billingauth.CredentialAPIKey }, billingauth.Scope{}),
		"a signed token":  as(func(id *billingauth.Identity) { id.Credential.Kind = billingauth.CredentialSignedToken }, billingauth.Scope{}),
		"an access token": as(func(id *billingauth.Identity) { id.Credential.Kind = billingauth.CredentialAccessToken }, billingauth.Scope{}),
		"a bound scope":   as(keep, authtest.MerchantScope(shop.MerchantID())),
	} {
		res := create(token, uuid.NewString(), course, embedded, nil)
		require.Equal(t, http.StatusForbidden, res.Code, "%s: %s", name, res.Body.String())
		require.Contains(t, res.Body.String(), `"customer_session_required"`, name)
	}
	// The request names no URL and no other mode; the checkout takes the
	// payment.
	refusedWith(create(aliceToken, uuid.NewString(), course, map[string]any{"ui_mode": "embedded", "success_url": "https://evil.e2e.test/"}, nil), http.StatusBadRequest, "unknown_field")
	refusedWith(create(aliceToken, uuid.NewString(), course, map[string]any{"ui_mode": "hosted"}, nil), http.StatusBadRequest, billing.CodeInvalidParam)
	res = call(app, http.MethodPost, "/v1/me/orders", aliceToken, map[string]any{"lines": []map[string]any{{"price_id": course}}, "checkout": embedded,
		"expected_total": "5000000", "payment": map[string]any{"token": gateway.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"})}}, map[string]string{"Idempotency-Key": uuid.NewString(), "Origin": origins[0]})
	refusedWith(res, http.StatusBadRequest, billing.CodeInvalidParam)
	// It returns to the registered origin the request came from, and to no other.
	for _, page := range []string{"", "https://evil.e2e.test", "https://shop.e2e.test.evil.test", "http://shop.e2e.test"} {
		refusedWith(create(aliceToken, uuid.NewString(), course, embedded, map[string]string{"Origin": page}), http.StatusBadRequest, "embed_origin_not_registered")
	}
	list := decode[billing.ListPage[billing.Order]](t, call(app, http.MethodGet, "/v1/me/orders", aliceToken, nil, nil))
	require.Empty(t, list.Items, "a refused checkout makes no order")

	// One call: the customer's order and its checkout, returning to the
	// registered origin it came from: each site of one merchant its own.
	key := uuid.NewString()
	order, firstSecret := withCheckout(create(aliceToken, key, course, map[string]any{}, map[string]string{"Origin": origins[1]}), http.StatusCreated)
	require.Equal(t, billing.CheckoutEmbedded, order.Checkout.UIMode)
	require.Equal(t, origins[1]+"/", order.Checkout.SuccessURL)
	require.True(t, order.Checkout.SavedPaymentMethods)
	require.Nil(t, order.Checkout.CancelURL)
	require.Equal(t, clock.Now().Add(hostedcheckout.MyOrderLifetime), order.Checkout.ExpiresAt)

	// The checkout host resolves it: that order alone, framed by the merchant's
	// origins, with the saved cards.
	shown := opens(firstSecret, order.ID)
	require.NotNil(t, shown.Checkout)
	require.Nil(t, shown.Checkout.URL)
	require.Equal(t, billing.CheckoutEmbedded, shown.Checkout.UIMode)
	require.Equal(t, origins[1]+"/", shown.Checkout.SuccessURL)
	require.True(t, shown.Checkout.SavedPaymentMethods)
	require.Equal(t, http.StatusOK, onHost(http.MethodGet, "/v1/me/payment-methods", firstSecret, nil, nil).Code)
	cfgRead := decode[billing.CustomerConfig](t, onHost(http.MethodGet, "/v1/me/config", firstSecret, nil, nil))
	require.Equal(t, origins, cfgRead.Merchant.EmbedOrigins)
	require.NotEqual(t, http.StatusOK, onHost(http.MethodGet, "/v1/me/orders/"+billing.OrderID(uuid.New()).String(), firstSecret, nil, nil).Code)
	page := onHost(http.MethodGet, "/c/"+order.ID.String(), "", nil, nil)
	require.Equal(t, http.StatusOK, page.Code)
	require.True(t, strings.HasSuffix(page.Header().Get("Content-Security-Policy"), "; frame-ancestors "+strings.Join(origins, " ")), page.Header().Get("Content-Security-Policy"))
	require.Empty(t, page.Header().Get("X-Frame-Options"))
	// A read shows the checkout, never its URL.
	read := decode[billing.Order](t, call(app, http.MethodGet, "/v1/me/orders/"+order.ID.String(), aliceToken, nil, nil))
	require.NotNil(t, read.Checkout)
	require.Nil(t, read.Checkout.URL)
	require.NotContains(t, read.Checkout.SuccessURL, "evil")

	// The order is idempotent; its URL is a fresh credential on every answer,
	// replacing the last. Without a checkout the retry answers the order alone.
	device := as(func(id *billingauth.Identity) { id.Credential.Kind = billingauth.CredentialDeviceKey }, billingauth.Scope{})
	replay := create(device, key, course, embedded, nil)
	require.Equal(t, "true", replay.Header().Get("Idempotent-Replayed"))
	again, secondSecret := withCheckout(replay, http.StatusCreated)
	require.Equal(t, order.ID, again.ID)
	require.NotEqual(t, firstSecret, secondSecret)
	refusedWith(onHost(http.MethodGet, "/v1/me/orders/"+order.ID.String(), firstSecret, nil, nil), http.StatusNotFound, "checkout_not_found")
	opens(secondSecret, order.ID)
	plain := create(aliceToken, key, course, nil, nil)
	require.Equal(t, http.StatusCreated, plain.Code, plain.Body.String())
	require.Equal(t, order.ID, decode[billing.Order](t, plain).ID)
	if c := decode[billing.Order](t, plain).Checkout; c != nil {
		require.Nil(t, c.URL)
	}
	// Another customer's request under the same key is their own order.
	bobs, _ := withCheckout(create(bobToken, key, course, embedded, nil), http.StatusCreated)
	require.NotEqual(t, order.ID, bobs.ID)
	require.Equal(t, origins[0]+"/", bobs.Checkout.SuccessURL)
	opens(secondSecret, order.ID)

	// A merchant's own checkout on an order of hers is never touched by hers.
	merchantOrder, err := shop.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: billing.CustomerID(uuid.MustParse(alice)),
		Lines: []billing.OrderLineParams{{PriceID: bundle}}, IdempotencyKey: uuid.NewString(),
		Checkout: &billing.OrderCheckoutParams{UIMode: billing.CheckoutEmbedded, SuccessURL: "https://shop.e2e.test/thanks"}})
	require.NoError(t, err)
	_, merchantSecret, ok := strings.Cut(*merchantOrder.Checkout.URL, "#")
	require.True(t, ok)
	_, paySecret := withCheckout(create(aliceToken, key, course, embedded, nil), http.StatusCreated)
	require.Equal(t, "https://shop.e2e.test/thanks", opens(merchantSecret, merchantOrder.ID).Checkout.SuccessURL)

	// The checkout pays its order on the checkout host; a paid order's retry
	// answers it without a URL.
	paid := onHost(http.MethodPost, "/v1/me/orders/"+order.ID.String()+"/pay", paySecret, map[string]any{
		"payment": map[string]any{
			"token":           gateway.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}),
			"billing_details": map[string]any{"name": "Card Holder", "address": map[string]string{"country": "US", "postal_code": "10001"}},
		},
		"expected_total": "5000000",
	}, map[string]string{"Idempotency-Key": uuid.NewString()})
	require.Equal(t, http.StatusOK, paid.Code, paid.Body.String())
	require.Equal(t, billing.OrderComplete, decode[billing.Order](t, paid).Status)
	after := create(aliceToken, key, course, embedded, nil)
	require.Equal(t, http.StatusCreated, after.Code, after.Body.String())
	require.Equal(t, billing.OrderComplete, decode[billing.Order](t, after).Status)
	require.Nil(t, decode[billing.Order](t, after).Checkout.URL)

	// A customer's checkout lasts its 15 minutes.
	bobsOrder, bobSecret := withCheckout(create(bobToken, uuid.NewString(), bundle, embedded, nil), http.StatusCreated)
	clock.Advance(hostedcheckout.MyOrderLifetime + time.Second)
	refusedWith(onHost(http.MethodGet, "/v1/me/orders/"+bobsOrder.ID.String(), bobSecret, nil, nil), http.StatusGone, "checkout_expired")
}

// A customer gets no checkout, and no order, where the deployment serves no
// checkout host or the merchant registered no origin to frame it.
func TestCustomerCheckoutNeedsItsHostAndOrigins(t *testing.T) {
	f := newFixture(t)
	cfg := f.config()
	cfg.ProviderWriteMode = openrails.ProviderWritesFull
	cfg.Merchants = map[string]openrails.MerchantDeclaration{"no-origins": {DisplayName: "No origins", PSPs: map[string]openrails.PSPConfig{"nmi": nmiPSP()}}}
	bill, shop := f.boot(t, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	auth := &authtest.Fake{}
	app := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(app, bill, openrails.Routes{Auth: auth}))
	product, err := shop.CreateProduct(t.Context(), billing.CreateProductParams{Key: "course", DisplayName: "Course"})
	require.NoError(t, err)
	price, err := shop.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "once", Currency: "USD", UnitAmount: 5_000_000})
	require.NoError(t, err)
	token := auth.Person(uuid.NewString())
	do := func(method string, body any) *httptest.ResponseRecorder {
		var payload bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&payload).Encode(body))
		}
		req := httptest.NewRequest(method, "/v1/me/orders", &payload)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", uuid.NewString())
		res := httptest.NewRecorder()
		app.ServeHTTP(res, req)
		return res
	}
	body := map[string]any{"lines": []map[string]any{{"price_id": price.ID}}, "checkout": map[string]any{"ui_mode": "embedded"}}
	res := do(http.MethodPost, body)
	require.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
	require.Contains(t, res.Body.String(), `"hosted_checkout_unavailable"`)

	_, _, err = bill.HostedCheckout("https://pay.e2e.test", nil)
	require.NoError(t, err)
	res = do(http.MethodPost, body)
	require.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
	require.Contains(t, res.Body.String(), `"checkout_embed_origins_required"`)
	require.Empty(t, decode[billing.ListPage[billing.Order]](t, do(http.MethodGet, nil)).Items)
}

func nmiPSP() openrails.PSPConfig {
	return openrails.NMIPSP{AccountID: "e2e-nmi", SecurityKey: "e2e-nmi-key", WebhookSigningSecret: "whsec_nmi", TokenizationKey: "e2e-tokenization"}.PSPConfig()
}
