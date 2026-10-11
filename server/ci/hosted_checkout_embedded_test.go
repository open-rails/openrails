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

// An embedded checkout needs the merchant's registered origins; its page is
// framed by them alone (frame-ancestors, no X-Frame-Options) while a hosted
// checkout's page is never framed. The origins are exact.
func TestHostedCheckoutEmbedded(t *testing.T) {
	s := newCheckoutShop(t)
	shop := handle(t, s.srv, s.mid)
	customer := billing.CustomerID(uuid.New())
	create := func(mode string) (billing.Order, string) {
		t.Helper()
		return s.order(customer, map[string]any{"success_url": "https://shop.e2e.test/thanks?order={ORDER_ID}", "ui_mode": mode})
	}
	page := func(order billing.Order) http.Header {
		t.Helper()
		res := s.call(checkoutHost, http.MethodGet, "/c/"+order.ID.String(), "", "", nil)
		require.Equal(t, http.StatusOK, res.Code)
		return res.Header()
	}

	res := s.call(apiHost, http.MethodPost, "/v1/app/orders", s.backend, uuid.NewString(), map[string]any{"customer_id": customer,
		"lines": []map[string]any{{"price_id": s.price()}}, "checkout": map[string]any{"success_url": "https://shop.e2e.test/thanks", "ui_mode": "embedded"}})
	refused(t, res, http.StatusBadRequest, "checkout_embed_origins_required")
	res = s.call(apiHost, http.MethodPost, "/v1/app/orders", s.backend, uuid.NewString(), map[string]any{"customer_id": customer,
		"lines": []map[string]any{{"price_id": s.price()}}, "checkout": map[string]any{"success_url": "https://shop.e2e.test/thanks", "ui_mode": "popup"}})
	refused(t, res, http.StatusBadRequest, "invalid_param")

	for _, origins := range [][]string{
		{"https://*.shop.e2e.test"}, {"https://shop.e2e.test/cart"}, {"http://shop.e2e.test"}, {"https://shop.e2e.test", "https://SHOP.e2e.test"},
		{"https://a.e2e.test", "https://b.e2e.test", "https://c.e2e.test", "https://d.e2e.test", "https://e.e2e.test", "https://f.e2e.test",
			"https://g.e2e.test", "https://h.e2e.test", "https://i.e2e.test", "https://j.e2e.test", "https://k.e2e.test"},
	} {
		_, err := shop.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{
			Settings: &billing.MerchantSettings{Checkout: &billing.MerchantCheckoutSettings{EmbedOrigins: origins}}})
		require.Error(t, err, "%v is refused", origins)
	}
	_, err := shop.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{
		Settings: &billing.MerchantSettings{Checkout: &billing.MerchantCheckoutSettings{EmbedOrigins: []string{"https://shop.e2e.test", "http://localhost:3000"}}}})
	require.NoError(t, err)

	embedded, secret := create("embedded")
	require.Equal(t, billing.CheckoutEmbedded, embedded.Checkout.UIMode)
	h := page(embedded)
	require.True(t, strings.HasSuffix(h.Get("Content-Security-Policy"), "; frame-ancestors https://shop.e2e.test http://localhost:3000"), h.Get("Content-Security-Policy"))
	require.Empty(t, h.Get("X-Frame-Options"), "X-Frame-Options cannot list origins")

	hosted, _ := create("")
	require.Equal(t, billing.CheckoutHosted, hosted.Checkout.UIMode)
	h = page(hosted)
	require.True(t, strings.HasSuffix(h.Get("Content-Security-Policy"), "; frame-ancestors 'none'"))
	require.Equal(t, "DENY", h.Get("X-Frame-Options"))

	// The page reads the merchant's origins to tell its frame it completed,
	// and its order reads its mode.
	cfg := decode[billing.PublicConfig](t, s.call(checkoutHost, http.MethodGet, "/v1/config", secret, "", nil))
	require.NotNil(t, cfg.Merchant)
	require.Equal(t, []string{"https://shop.e2e.test", "http://localhost:3000"}, cfg.Merchant.EmbedOrigins)
	read := decode[billing.Order](t, s.call(checkoutHost, http.MethodGet, "/v1/me/orders/"+embedded.ID.String(), secret, "", nil))
	require.Equal(t, billing.CheckoutEmbedded, read.Checkout.UIMode)

	// Another merchant's page, or none, is framed by nobody.
	h = page(billing.Order{ID: billing.OrderID(uuid.New())})
	require.Equal(t, "DENY", h.Get("X-Frame-Options"))

	// Origins withdrawn: the page is framed by none, at once.
	_, err = shop.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{
		Settings: &billing.MerchantSettings{Checkout: &billing.MerchantCheckoutSettings{EmbedOrigins: []string{}}}})
	require.NoError(t, err)
	h = page(embedded)
	require.True(t, strings.HasSuffix(h.Get("Content-Security-Policy"), "; frame-ancestors 'none'"))
	require.Equal(t, "DENY", h.Get("X-Frame-Options"))
}
