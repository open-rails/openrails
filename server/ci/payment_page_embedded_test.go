//go:build e2e && integration

package ci_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// An embedded payment page needs the merchant's registered origins; its page is
// framed by them alone (frame-ancestors, no X-Frame-Options) while a hosted
// pay URL's page is never framed. The origins are exact.
func TestPaymentPageEmbedded(t *testing.T) {
	s := newPayShop(t)
	shop := handle(t, s.srv, s.mid)
	customer := billing.CustomerID(uuid.New())
	create := func(embedded bool) (billing.Order, string) {
		t.Helper()
		return s.order(customer, map[string]any{"success_url": "https://shop.e2e.test/thanks?order={ORDER_ID}", "embedded": embedded})
	}
	page := func(order billing.Order) http.Header {
		t.Helper()
		res := s.call(paymentHost, http.MethodGet, "/pay/"+order.ID.String(), "", "", nil)
		require.Equal(t, http.StatusOK, res.Code)
		return res.Header()
	}

	res := s.call(serverHost, http.MethodPost, "/v1/app/orders", s.backend, uuid.NewString(), map[string]any{"customer_id": customer,
		"lines": []map[string]any{{"price_id": s.price()}}, "pay_url": map[string]any{"success_url": "https://shop.e2e.test/thanks", "embedded": true}})
	refused(t, res, http.StatusBadRequest, "embed_origins_required")
	res = s.call(serverHost, http.MethodPost, "/v1/app/orders", s.backend, uuid.NewString(), map[string]any{"customer_id": customer,
		"lines": []map[string]any{{"price_id": s.price()}}, "pay_url": map[string]any{"success_url": "https://shop.e2e.test/thanks", "embedded": "popup"}})
	refused(t, res, http.StatusBadRequest, "invalid_param")

	for _, origins := range [][]string{
		{"https://*.shop.e2e.test"}, {"https://shop.e2e.test/cart"}, {"http://shop.e2e.test"}, {"https://shop.e2e.test", "https://SHOP.e2e.test"},
		{"https://a.e2e.test", "https://b.e2e.test", "https://c.e2e.test", "https://d.e2e.test", "https://e.e2e.test", "https://f.e2e.test",
			"https://g.e2e.test", "https://h.e2e.test", "https://i.e2e.test", "https://j.e2e.test", "https://k.e2e.test"},
	} {
		_, err := shop.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{
			MerchantSettings: &billing.MerchantSettings{Checkout: &billing.MerchantCheckoutSettings{EmbedOrigins: origins}}})
		require.Error(t, err, "%v is refused", origins)
	}
	_, err := shop.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{
		MerchantSettings: &billing.MerchantSettings{Checkout: &billing.MerchantCheckoutSettings{EmbedOrigins: []string{"https://shop.e2e.test", "http://localhost:3000"}}}})
	require.NoError(t, err)

	embedded, secret := create(true)
	require.True(t, embedded.Embedded)
	h := page(embedded)
	require.True(t, strings.HasSuffix(h.Get("Content-Security-Policy"), "; frame-ancestors https://shop.e2e.test http://localhost:3000"), h.Get("Content-Security-Policy"))
	require.Empty(t, h.Get("X-Frame-Options"), "X-Frame-Options cannot list origins")

	hosted, _ := create(false)
	require.False(t, hosted.Embedded)
	h = page(hosted)
	require.True(t, strings.HasSuffix(h.Get("Content-Security-Policy"), "; frame-ancestors 'none'"))
	require.Equal(t, "DENY", h.Get("X-Frame-Options"))

	// The page reads the merchant's origins to tell its frame it completed,
	// and its order reads its mode.
	cfg := decode[billing.CustomerConfig](t, s.call(paymentHost, http.MethodGet, "/v1/me/config", secret, "", nil))
	require.Equal(t, []string{"https://shop.e2e.test", "http://localhost:3000"}, cfg.Merchant.EmbedOrigins)
	read := decode[billing.Order](t, s.call(paymentHost, http.MethodGet, "/v1/me/orders/"+embedded.ID.String(), secret, "", nil))
	require.True(t, read.Embedded)

	// Another merchant's page, or none, is framed by nobody.
	h = page(billing.Order{ID: billing.OrderID(uuid.New())})
	require.Equal(t, "DENY", h.Get("X-Frame-Options"))

	// Origins withdrawn: the page is framed by none, at once.
	_, err = shop.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{
		MerchantSettings: &billing.MerchantSettings{Checkout: &billing.MerchantCheckoutSettings{EmbedOrigins: []string{}}}})
	require.NoError(t, err)
	h = page(embedded)
	require.True(t, strings.HasSuffix(h.Get("Content-Security-Policy"), "; frame-ancestors 'none'"))
	require.Equal(t, "DENY", h.Get("X-Frame-Options"))
}
