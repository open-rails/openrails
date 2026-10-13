//go:build e2e && integration

package ci_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/openrailstest/nmimock"
	"github.com/stretchr/testify/require"
)

// Independent embedded hosts share the canonical order capability. A publishing
// app needs no payment page assets; only the isolated payment host serves the page.
func TestEmbeddedEnginePaymentPage(t *testing.T) {
	f := newFixture(t)
	gateway := nmimock.New(nmimock.Options{})
	t.Cleanup(gateway.Close)
	cfg := f.config()
	cfg.ProviderWriteMode = openrails.ProviderWritesFull
	cfg.PublicBillingBaseURL = "https://api.e2e.test"
	cfg.ReturnOrigins = []string{"https://shop.e2e.test"}
	cfg.ProviderSandbox = &openrails.ProviderSandboxConfig{NMIGatewayURL: gateway.URL()}
	cfg.Merchants = map[string]openrails.MerchantDeclaration{"shared-pay": {Sandbox: true,
		DisplayName: "Shared pay", Issuer: "https://identity.e2e.test",
		MerchantSettings: billing.MerchantSettings{PaymentPage: &billing.MerchantPaymentPageSettings{EmbedOrigins: []string{"https://shop.e2e.test"}}},
		PSPs:             map[string]openrails.PSPConfig{"nmi": openrails.NMIPSP{Environment: openrails.EnvironmentSandbox, GatewayID: "test", SecurityKey: "test", WebhookSigningSecret: "test", TokenizationKey: "test"}.PSPConfig()},
	}}
	publisher, shop := f.boot(t, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	for _, origin := range []string{"http://pay.e2e.test", "https://api.e2e.test", "https://identity.e2e.test", "https://pay.e2e.test/path"} {
		_, _, err := publisher.PaymentPage(origin, nil)
		require.Error(t, err, origin)
	}
	origin, handler, err := publisher.PaymentPage("https://pay.e2e.test", nil)
	require.NoError(t, err)
	require.Equal(t, "https://pay.e2e.test", origin)
	require.Nil(t, handler)
	_, _, err = publisher.PaymentPage("https://other.e2e.test", nil)
	require.Error(t, err)
	serving, _ := f.boot(t, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	_, _, err = serving.PaymentPage("https://pay.e2e.test", fstest.MapFS{})
	require.Error(t, err, "missing assets must not install an origin")
	origin, handler, err = serving.PaymentPage("https://pay.e2e.test", paymentPage)
	require.NoError(t, err)
	require.Equal(t, "https://pay.e2e.test", origin)
	require.NotNil(t, handler)
	product, err := shop.CreateProduct(t.Context(), billing.CreateProductParams{Key: "embedded-item", DisplayName: "Item"})
	require.NoError(t, err)
	price, err := shop.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "usd", Currency: "USD", UnitAmount: 5_000_000})
	require.NoError(t, err)
	order, err := shop.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: billing.CustomerID(uuid.New()), Lines: []billing.OrderLineParams{{PriceID: price.ID}}, IdempotencyKey: uuid.NewString(), PayURL: &billing.OrderPayURLParams{Embedded: true, SuccessURL: "https://shop.e2e.test/thanks"}})
	require.NoError(t, err)
	require.NotNil(t, order.PayURLExpiresAt)
	require.NotNil(t, order.PayURL)
	page, secret, ok := strings.Cut(*order.PayURL, "#")
	require.True(t, ok)
	require.Equal(t, "https://pay.e2e.test/pay/"+order.ID.String(), page)
	call := func(path, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "https://pay.e2e.test"+path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	res := call("/pay/"+order.ID.String(), "")
	require.Equal(t, 200, res.Code)
	require.Contains(t, res.Header().Get("Content-Security-Policy"), "frame-ancestors https://shop.e2e.test")
	res = call("/v1/me/orders/"+order.ID.String(), secret)
	require.Equal(t, 200, res.Code, res.Body.String())
	res = call("/v1/me/orders/"+order.ID.String(), "")
	require.Equal(t, 404, res.Code, "an absent capability reveals no order")
	res = call("/v1/me/orders/"+billing.OrderID(uuid.New()).String(), secret)
	require.NotEqual(t, 200, res.Code)
	for _, path := range []string{"/admin/", "/auth/v1/me", "/v1/catalog/products", "/v1/me/payment-methods"} {
		res = call(path, secret)
		require.NotEqual(t, 200, res.Code, path)
	}

	late, _ := f.boot(t, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	require.NoError(t, late.Start(t.Context()))
	_, _, err = late.PaymentPage("https://late.e2e.test", nil)
	require.ErrorContains(t, err, "before Start")
	require.NoError(t, late.Close(t.Context()))
	_, _, err = late.PaymentPage("https://late.e2e.test", nil)
	require.ErrorContains(t, err, "closed")
}
