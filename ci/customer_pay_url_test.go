//go:build e2e && integration

package ci_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/paymentpage"
	"github.com/open-rails/openrails/openrailstest/nmimock"
)

func decode[T any](t *testing.T, res *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out), res.Body.String())
	return out
}

// A host's customer buys through billing-ui alone, in the shared-payment-origin
// topology: one app mounts the customer routes and issues pay URLs on the
// payment origin another app serves, over one database. The signed-in person
// asks for its order with a pay URL and gets the URL in the same answer; the
// payment origin resolves it, frames it by the merchant's origins and takes the
// payment. The pay URL returns nowhere (the frame tells its page) and lasts
// as long as its order. Asking again for the same lines, from any tab, reload
// or click, answers the customer's one open order with a fresh URL, ending the
// last; a paid, expired or owned order does not. Only the merchant's own
// person in a session of their own asks; a merchant's own pay URL is never
// touched.
func TestCustomerOrderWithEmbeddedPayment(t *testing.T) {
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
	cfg.Merchants = map[string]openrails.MerchantDeclaration{"shared-pay": {
		DisplayName:      "Shared pay",
		MerchantSettings: billing.MerchantSettings{PaymentPage: &billing.MerchantPaymentPageSettings{EmbedOrigins: origins}},
		PSPs:             map[string]openrails.PSPConfig{"nmi": nmiPSP()},
	}}
	deps := openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool, Clock: clock}
	publisher, shop := f.boot(t, cfg, deps)
	_, none, err := publisher.PaymentPage("https://pay.e2e.test", nil)
	require.NoError(t, err)
	require.Nil(t, none, "the publishing app issues URLs and serves no page")
	serving, _ := f.boot(t, cfg, deps)
	_, paymentHost, err := serving.PaymentPage("https://pay.e2e.test", fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>Payment</title>")},
		"assets/app.js": {Data: []byte("void 0")},
	})
	require.NoError(t, err)

	auth := &authtest.Fake{}
	app := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(app, publisher, openrails.Routes{Auth: auth}))
	sell := func(key string, amount int64, ownership catalog.Ownership) billing.PriceID {
		t.Helper()
		product, err := shop.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Ownership: ownership})
		require.NoError(t, err)
		price, err := shop.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "once", Currency: "USD", UnitAmount: amount})
		require.NoError(t, err)
		return price.ID
	}
	course, bundle, pack := sell("course", 5_000_000, ""), sell("bundle", 9_000_000, ""), sell("pack", 3_000_000, catalog.OwnershipConsumable)

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
	embedded := map[string]any{"embedded": true}
	// order asks for the customer's order of price, as billing-ui does: no
	// key.
	order := func(token string, price billing.PriceID, payURL any, header map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		body := map[string]any{"lines": []map[string]any{{"price_id": price}}}
		if payURL != nil {
			body["pay_url"] = payURL
		}
		return call(app, http.MethodPost, "/v1/me/orders", token, body, header)
	}
	// withPayURL is the answer's order and its pay URL's secret.
	withPayURL := func(res *httptest.ResponseRecorder, status int) (billing.Order, string) {
		t.Helper()
		require.Equal(t, status, res.Code, res.Body.String())
		require.Equal(t, "no-store", res.Header().Get("Cache-Control"))
		order := decode[billing.Order](t, res)
		require.Equal(t, billing.OrderOriginCustomer, order.Origin)
		require.NotNil(t, order.PayURLExpiresAt)
		require.NotNil(t, order.PayURL)
		page, secret, ok := strings.Cut(*order.PayURL, "#")
		require.True(t, ok)
		require.Equal(t, "https://pay.e2e.test/pay/"+order.ID.String(), page)
		require.True(t, strings.HasPrefix(secret, paymentpage.SecretPrefix))
		return order, secret
	}
	onHost := func(method, path, secret string, body any, header map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		return call(paymentHost, method, "https://pay.e2e.test"+path, secret, body, header)
	}
	opens := func(secret string, id billing.OrderID) billing.Order {
		t.Helper()
		res := onHost(http.MethodGet, "/v1/me/orders/"+id.String(), secret, nil, nil)
		require.Equal(t, http.StatusOK, res.Code, res.Body.String())
		return decode[billing.Order](t, res)
	}
	pay := func(secret string, id billing.OrderID, total string) {
		t.Helper()
		res := onHost(http.MethodPost, "/v1/me/orders/"+id.String()+"/pay", secret, map[string]any{
			"payment": map[string]any{
				"token":           gateway.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}),
				"billing_details": map[string]any{"name": "Card Holder", "address": map[string]string{"country": "US", "postal_code": "10001"}},
			},
			"expected_total": total,
		}, map[string]string{"Idempotency-Key": uuid.NewString()})
		require.Equal(t, http.StatusOK, res.Code, res.Body.String())
		require.Equal(t, billing.OrderComplete, decode[billing.Order](t, res).Status)
	}

	alice, bob := uuid.NewString(), uuid.NewString()
	aliceToken, bobToken := auth.Person(alice), auth.Person(bob)

	// The browser never reads the catalog.
	res := call(app, http.MethodGet, "/v1/products?"+url.Values{"merchant": {shop.MerchantID().String()}, "keys": {"course"}}.Encode(), "", nil, nil)
	require.Equal(t, http.StatusNotFound, res.Code, res.Body.String())

	// Only the merchant's own person, signed in, asks for a pay URL.
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
		res := order(token, course, embedded, nil)
		require.Equal(t, http.StatusForbidden, res.Code, "%s: %s", name, res.Body.String())
		require.Contains(t, res.Body.String(), `"`+billing.CodeInvokerScopedPrincipal+`"`, name)
	}
	for name, token := range map[string]string{
		"an API key":      as(func(id *billingauth.Identity) { id.Credential.Kind = billingauth.CredentialAPIKey }, billingauth.Scope{}),
		"a signed token":  as(func(id *billingauth.Identity) { id.Credential.Kind = billingauth.CredentialSignedToken }, billingauth.Scope{}),
		"an access token": as(func(id *billingauth.Identity) { id.Credential.Kind = billingauth.CredentialAccessToken }, billingauth.Scope{}),
		"a bound scope":   as(keep, authtest.MerchantScope(shop.MerchantID())),
	} {
		res := order(token, course, embedded, nil)
		require.Equal(t, http.StatusForbidden, res.Code, "%s: %s", name, res.Body.String())
		require.Contains(t, res.Body.String(), `"customer_session_required"`, name)
	}
	// The request names no URL and no other mode; the pay URL takes the
	// payment.
	for _, field := range []string{"success_url", "cancel_url", "return_path"} {
		refusedWith(order(aliceToken, course, map[string]any{"embedded": true, field: "https://evil.e2e.test/"}, nil), http.StatusBadRequest, "unknown_field")
	}
	refusedWith(order(aliceToken, course, map[string]any{"embedded": false}, nil), http.StatusBadRequest, billing.CodeInvalidParam)
	res = call(app, http.MethodPost, "/v1/me/orders", aliceToken, map[string]any{"lines": []map[string]any{{"price_id": course}}, "pay_url": embedded,
		"expected_total": "5000000", "payment": map[string]any{"token": gateway.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"})}}, map[string]string{"Idempotency-Key": uuid.NewString(), "Origin": origins[0]})
	refusedWith(res, http.StatusBadRequest, billing.CodeInvalidParam)
	list := decode[billing.ListPage[billing.Order]](t, call(app, http.MethodGet, "/v1/me/orders", aliceToken, nil, nil))
	require.Empty(t, list.Items, "a refused pay URL makes no order")

	// One call, no key: the customer's order and its pay URL, which returns
	// nowhere and lasts as long as the order (24 hours).
	first, firstSecret := withPayURL(order(aliceToken, course, embedded, nil), http.StatusCreated)
	require.True(t, first.Embedded)
	require.Nil(t, first.SuccessURL)
	require.Nil(t, first.CancelURL)
	require.True(t, first.SavedPaymentMethods)
	require.NotNil(t, first.ExpiresAt)
	require.Equal(t, clock.Now().Add(orders.CustomerTTL), *first.ExpiresAt)
	require.Equal(t, *first.ExpiresAt, *first.PayURLExpiresAt)

	// The payment origin resolves it: that order alone, framed by the merchant's
	// origins, with the saved cards.
	shown := opens(firstSecret, first.ID)
	require.Nil(t, shown.PayURL)
	require.Nil(t, shown.SuccessURL)
	require.True(t, shown.SavedPaymentMethods)
	require.Equal(t, http.StatusOK, onHost(http.MethodGet, "/v1/me/payment-methods", firstSecret, nil, nil).Code)
	cfgRead := decode[billing.CustomerConfig](t, onHost(http.MethodGet, "/v1/me/config", firstSecret, nil, nil))
	require.Equal(t, origins, cfgRead.Merchant.EmbedOrigins)
	require.NotEqual(t, http.StatusOK, onHost(http.MethodGet, "/v1/me/orders/"+billing.OrderID(uuid.New()).String(), firstSecret, nil, nil).Code)
	page := onHost(http.MethodGet, "/pay/"+first.ID.String(), "", nil, nil)
	require.Equal(t, http.StatusOK, page.Code)
	require.True(t, strings.HasSuffix(page.Header().Get("Content-Security-Policy"), "; frame-ancestors "+strings.Join(origins, " ")), page.Header().Get("Content-Security-Policy"))
	require.Empty(t, page.Header().Get("X-Frame-Options"))
	read := decode[billing.Order](t, call(app, http.MethodGet, "/v1/me/orders/"+first.ID.String(), aliceToken, nil, nil))
	require.Nil(t, read.PayURL, "a read never shows the URL")

	// Asking again (a reload, another tab, a device key) answers the same
	// order with a fresh URL; the last stops working. Without a pay URL it
	// answers the order alone.
	device := as(func(id *billingauth.Identity) { id.Credential.Kind = billingauth.CredentialDeviceKey }, billingauth.Scope{})
	again, secondSecret := withPayURL(order(device, course, embedded, nil), http.StatusOK)
	require.Equal(t, first.ID, again.ID)
	refusedWith(onHost(http.MethodGet, "/v1/me/orders/"+first.ID.String(), firstSecret, nil, nil), http.StatusNotFound, "pay_url_not_found")
	opens(secondSecret, first.ID)
	plain := order(aliceToken, course, nil, nil)
	require.Equal(t, http.StatusOK, plain.Code, plain.Body.String())
	require.Equal(t, first.ID, decode[billing.Order](t, plain).ID)
	require.Nil(t, decode[billing.Order](t, plain).PayURL)
	// Another price is another order; another customer's request is theirs.
	packOrder, packSecret := withPayURL(order(aliceToken, pack, embedded, nil), http.StatusCreated)
	require.NotEqual(t, first.ID, packOrder.ID)

	// Concurrent clicks make one order.
	answers := make([]*httptest.ResponseRecorder, 8)
	var wg sync.WaitGroup
	for i := range answers {
		wg.Go(func() { answers[i] = order(bobToken, pack, embedded, nil) })
	}
	wg.Wait()
	created, ids := 0, map[billing.OrderID]bool{}
	for _, res := range answers {
		require.Contains(t, []int{http.StatusCreated, http.StatusOK}, res.Code, res.Body.String())
		if res.Code == http.StatusCreated {
			created++
		}
		ids[decode[billing.Order](t, res).ID] = true
	}
	require.Equal(t, 1, created)
	require.Len(t, ids, 1)
	for id := range ids {
		require.NotEqual(t, packOrder.ID, id)
	}

	// A merchant's own pay URL on an order of hers is never touched by hers.
	merchantOrder, err := shop.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: billing.CustomerID(uuid.MustParse(alice)),
		Lines: []billing.OrderLineParams{{PriceID: bundle}}, IdempotencyKey: uuid.NewString(),
		PayURL: &billing.OrderPayURLParams{Embedded: true, SuccessURL: "https://shop.e2e.test/thanks"}})
	require.NoError(t, err)
	_, merchantSecret, ok := strings.Cut(*merchantOrder.PayURL, "#")
	require.True(t, ok)
	_, paySecret := withPayURL(order(aliceToken, course, embedded, nil), http.StatusOK)
	require.Equal(t, "https://shop.e2e.test/thanks", *opens(merchantSecret, merchantOrder.ID).SuccessURL)

	// Paid, the course is owned: asking again is already_owned. A consumable
	// paid is bought again as a new order.
	pay(paySecret, first.ID, "5000000")
	refusedWith(order(aliceToken, course, embedded, nil), http.StatusConflict, "already_owned")
	pay(packSecret, packOrder.ID, "3000000")
	next, _ := withPayURL(order(aliceToken, pack, embedded, nil), http.StatusCreated)
	require.NotEqual(t, packOrder.ID, next.ID)

	// A pay URL lasts as long as its order, whose expiry asking again never
	// slides: past it the pay URL is gone, and asking again makes a new order.
	// (Moving the clock comes last: the harness settles no payment after it.)
	var bobOrder billing.OrderID
	for id := range ids {
		bobOrder = id
	}
	bobs, _ := withPayURL(order(bobToken, pack, embedded, nil), http.StatusOK)
	clock.Advance(time.Hour)
	later, bobSecret := withPayURL(order(bobToken, pack, embedded, nil), http.StatusOK)
	require.Equal(t, bobOrder, later.ID)
	require.Equal(t, bobs.PayURLExpiresAt, later.PayURLExpiresAt, "asking again never slides the order's expiry")
	clock.Advance(bobs.PayURLExpiresAt.Sub(clock.Now()) - time.Minute)
	opens(bobSecret, bobOrder)
	clock.Advance(2 * time.Minute)
	refusedWith(onHost(http.MethodGet, "/v1/me/orders/"+bobOrder.String(), bobSecret, nil, nil), http.StatusGone, "pay_url_expired")
	renewed, _ := withPayURL(order(bobToken, pack, embedded, nil), http.StatusCreated)
	require.NotEqual(t, bobOrder, renewed.ID)
}

// A customer's embedded payment page returns nowhere; a redirect PSP (Stripe's
// hosted page) started in its frame comes back to the frame's own page on the
// payment origin, and to no other site.
func TestCustomerPayURLRedirectReturnsToItsPage(t *testing.T) {
	f := newFixture(t)
	provider := &stripeCheckoutFake{t: t}
	cfg := f.config()
	cfg.ProviderWriteMode = openrails.ProviderWritesFull
	cfg.RateLimitsDisabled = true
	cfg.Merchants = map[string]openrails.MerchantDeclaration{"redirect-" + uuid.NewString()[:8]: {
		DisplayName:      "Redirect shop",
		MerchantSettings: billing.MerchantSettings{PaymentPage: &billing.MerchantPaymentPageSettings{EmbedOrigins: []string{"https://shop.e2e.test"}}},
		PSPs: map[string]openrails.PSPConfig{"stripe": {Rail: "stripe", Environment: "test", AccountID: "acct_e2e",
			Secrets: map[string]string{"secret_key": "sk_test_e2e", "webhook_signing_secret": "whsec_e2e"}}},
	}}
	bill, shop := f.boot(t, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool, StripeTransport: provider})
	_, paymentHost, err := bill.PaymentPage("https://pay.e2e.test", fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>Payment</title>")},
		"assets/app.js": {Data: []byte("void 0")},
	})
	require.NoError(t, err)
	auth := &authtest.Fake{}
	app := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(app, bill, openrails.Routes{Auth: auth}))
	product, err := shop.CreateProduct(t.Context(), billing.CreateProductParams{Key: "post", DisplayName: "Post"})
	require.NoError(t, err)
	price, err := shop.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "once", Currency: "USD", UnitAmount: 1_000_000})
	require.NoError(t, err)
	call := func(h http.Handler, target, bearer string, body any, key string) *httptest.ResponseRecorder {
		var payload bytes.Buffer
		require.NoError(t, json.NewEncoder(&payload).Encode(body))
		req := httptest.NewRequest(http.MethodPost, target, &payload)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		return res
	}

	res := call(app, "/v1/me/orders", auth.Person(uuid.NewString()), map[string]any{"lines": []map[string]any{{"price_id": price.ID}}, "pay_url": map[string]any{"embedded": true}}, "")
	require.Equal(t, http.StatusCreated, res.Code, res.Body.String())
	order := decode[billing.Order](t, res)
	require.Nil(t, order.SuccessURL, "an embedded customer pay URL returns nowhere")
	require.Nil(t, order.CancelURL)
	_, secret, ok := strings.Cut(*order.PayURL, "#")
	require.True(t, ok)
	page := "https://pay.e2e.test" + paymentpage.PagePath(order.ID)
	pay := func(returnURL string) *httptest.ResponseRecorder {
		return call(paymentHost, "https://pay.e2e.test/v1/me/orders/"+order.ID.String()+"/pay", secret,
			map[string]any{"payment": map[string]any{"return_url": returnURL}, "expected_total": "1000000"}, uuid.NewString())
	}
	for _, elsewhere := range []string{"https://evil.e2e.test/pay/" + order.ID.String(), "https://pay.e2e.test/admin", "https://shop.e2e.test/thanks"} {
		res := pay(elsewhere)
		require.Equal(t, http.StatusBadRequest, res.Code, "%s: %s", elsewhere, res.Body.String())
	}
	require.Zero(t, provider.purchaseCalls.Load(), "a refused return URL never reaches Stripe")

	res = pay(page)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	paying := decode[billing.Order](t, res)
	require.Equal(t, billing.OrderPaymentRequiresAction, paying.Payment.Status)
	require.Equal(t, "redirect_to_url", paying.Payment.NextAction.Type)
	provider.mu.Lock()
	success, cancel := provider.purchase.Get("success_url"), provider.purchase.Get("cancel_url")
	provider.mu.Unlock()
	require.True(t, strings.HasPrefix(success, page), "Stripe returns to the frame's page: %s", success)
	require.True(t, strings.HasPrefix(cancel, page), cancel)
}

// A customer gets no pay URL, and no order, where the deployment serves no
// payment origin or the merchant registered no origin to frame it.
func TestCustomerPayURLNeedsItsHostAndOrigins(t *testing.T) {
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
	body := map[string]any{"lines": []map[string]any{{"price_id": price.ID}}, "pay_url": map[string]any{"embedded": true}}
	res := do(http.MethodPost, body)
	require.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
	require.Contains(t, res.Body.String(), `"pay_url_unavailable"`)

	_, _, err = bill.PaymentPage("https://pay.e2e.test", nil)
	require.NoError(t, err)
	res = do(http.MethodPost, body)
	require.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
	require.Contains(t, res.Body.String(), `"embed_origins_required"`)
	require.Empty(t, decode[billing.ListPage[billing.Order]](t, do(http.MethodGet, nil)).Items)
}

func nmiPSP() openrails.PSPConfig {
	return openrails.NMIPSP{Environment: openrails.EnvironmentSandbox, GatewayID: "e2e-nmi", SecurityKey: "e2e-nmi-key", WebhookSigningSecret: "whsec_nmi", TokenizationKey: "e2e-tokenization"}.PSPConfig()
}
