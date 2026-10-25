//go:build integration

package critical_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

func hostCheckoutPage(t *testing.T, w *world) *httptest.Server {
	t.Helper()
	assets := os.Getenv("OPENRAILS_CRITICAL_PAYMENT_ASSETS")
	require.NotEmpty(t, assets, "built real payment assets are mandatory")
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	canonical, handler, err := w.engine.CheckoutPage(origin, os.DirFS(assets))
	require.NoError(t, err)
	require.Equal(t, origin, canonical)
	require.NotNil(t, handler)
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	return server
}
func hostPageRequest(t *testing.T, server *httptest.Server, method, path, token string, body any, headers ...map[string]string) (int, http.Header, []byte) {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, bytes.NewReader(data))
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, extra := range headers {
		for k, v := range extra {
			req.Header.Set(k, v)
		}
	}
	response, err := server.Client().Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, response.Header, raw
}
func hostPayOrder(t *testing.T, a *authorityWorld, key string, embedded bool) (*billing.Order, string) {
	t.Helper()
	price := hostProduct(t, a.alpha, key)
	order, err := a.alpha.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Lines: []billing.OrderLineParams{{PriceID: price.ID}}, IdempotencyKey: key, PayURL: &billing.OrderPayURLParams{SuccessURL: "https://critical.invalid/done?order={ORDER_ID}", Embedded: embedded}})
	require.NoError(t, err)
	require.NotNil(t, order.PayURL)
	address, err := url.Parse(*order.PayURL)
	require.NoError(t, err)
	require.NotEmpty(t, address.Fragment)
	return order, address.Fragment
}
func TestHostedCheckout(t *testing.T) {
	t.Run("OR-J03-S01/capability-pays-only-its-order", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		page := hostCheckoutPage(t, a.world)
		order, secret := hostPayOrder(t, a, "hosted", false)
		status, headers, raw := hostPageRequest(t, page, "GET", "/v1/me/orders/"+order.ID.String(), secret, nil)
		require.Equal(t, 200, status, string(raw))
		require.Equal(t, "no-store", headers.Get("Cache-Control"))
		var found billing.Order
		require.NoError(t, json.Unmarshal(raw, &found))
		require.Equal(t, order.ID, found.ID)
		require.EqualValues(t, 10_000_000, found.Total)
		status, _, raw = hostPageRequest(t, page, "GET", "/v1/me/config", secret, nil)
		require.Equal(t, 200, status, string(raw))
		status, _, raw = hostPageRequest(t, page, "GET", "/v1/me/payment-methods", secret, nil)
		require.Equal(t, 404, status, string(raw))
		token := p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})
		status, _, raw = hostPageRequest(t, page, "POST", "/v1/me/orders/"+order.ID.String()+"/pay", secret, billing.PayOrderParams{Payment: billing.OrderPaymentParams{Token: token}, ExpectedTotal: 10_000_000}, map[string]string{"Idempotency-Key": "hosted-pay"})
		require.Equal(t, 200, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &found))
		require.Equal(t, billing.OrderComplete, found.Status)
		require.Len(t, p.effects(t), 1)
		require.EqualValues(t, 1000, p.effects(t)[0].Amount)
		canonical, err := a.alpha.GetOrder(t.Context(), order.ID)
		require.NoError(t, err)
		require.Equal(t, found.Payment.PaymentID, canonical.Payment.PaymentID)
	})
	t.Run("OR-J03-S02/foreign-tampered-and-expired-capability-refused", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		page := hostCheckoutPage(t, a.world)
		first, secret := hostPayOrder(t, a, "first", false)
		second, secondSecret := hostPayOrder(t, a, "second", false)
		for _, trial := range []struct {
			id    billing.OrderID
			token string
		}{{first.ID, secondSecret}, {second.ID, secret}, {first.ID, secret[:len(secret)-2] + "XX"}, {first.ID, ""}} {
			status, _, raw := hostPageRequest(t, page, "GET", "/v1/me/orders/"+trial.id.String(), trial.token, nil)
			require.Equal(t, 404, status, string(raw))
		}
		a.clock.Advance(first.PayURLExpiresAt.Sub(a.clock.Now()))
		status, _, raw := hostPageRequest(t, page, "GET", "/v1/me/orders/"+first.ID.String(), secret, nil)
		require.Equal(t, 410, status, string(raw))
		require.Contains(t, string(raw), "pay_url_expired")
		require.Empty(t, p.effects(t))
		money, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID))})
		require.NoError(t, err)
		require.Empty(t, money.Items)
	})
	t.Run("OR-J03-S03/checkout-and-api-authority-stay-separate", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		page := hostCheckoutPage(t, a.world)
		_, err := a.alpha.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{MerchantSettings: &billing.MerchantSettings{Checkout: &billing.MerchantCheckoutSettings{EmbedOrigins: []string{"https://merchant.critical.invalid"}}}})
		require.NoError(t, err)
		order, secret := hostPayOrder(t, a, "embedded", true)
		for _, token := range []string{secret, a.alphaKey.Secret, a.aliceToken} {
			status, _, raw := hostPageRequest(t, page, "GET", "/v1/admin/customers", token, nil)
			require.Equal(t, 404, status, string(raw))
		}
		status, raw := a.request(t, "GET", "/v1/admin/customers", secret, nil)
		require.Equal(t, 401, status, string(raw))
		status, headers, raw := hostPageRequest(t, page, "GET", "/pay/"+order.ID.String(), "", nil, map[string]string{"Origin": "https://evil.critical.invalid"})
		require.Equal(t, 200, status, string(raw))
		csp := headers.Get("Content-Security-Policy")
		require.Contains(t, csp, "frame-ancestors https://merchant.critical.invalid")
		require.NotContains(t, csp, "evil.critical.invalid")
		require.Empty(t, headers.Get("Access-Control-Allow-Origin"))
	})
	t.Run("OR-J03-S05/real-assets-and-config-redact-private-state", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		page := hostCheckoutPage(t, a.world)
		order, secret := hostPayOrder(t, a, "assets", false)
		status, headers, html := hostPageRequest(t, page, "GET", "/pay/"+order.ID.String(), "", nil)
		require.Equal(t, 200, status, string(html))
		require.Contains(t, headers.Get("Content-Security-Policy"), "frame-ancestors 'none'")
		require.Equal(t, "no-referrer", headers.Get("Referrer-Policy"))
		paths := regexp.MustCompile(`(?:src|href)="(/assets/[^" ]+)"`).FindAllSubmatch(html, -1)
		require.NotEmpty(t, paths, "the actual built payment app must be loaded")
		blobs := [][]byte{html}
		for _, path := range paths {
			status, _, raw := hostPageRequest(t, page, "GET", string(path[1]), "", nil)
			require.Equal(t, 200, status, string(raw))
			require.NotEmpty(t, raw)
			blobs = append(blobs, raw)
		}
		for _, path := range []string{"/v1/config", "/v1/me/config"} {
			status, _, raw := hostPageRequest(t, page, "GET", path, secret, nil)
			require.Equal(t, 200, status, string(raw))
			blobs = append(blobs, raw)
		}
		for _, blob := range blobs {
			for _, private := range []string{secret, a.alphaKey.Secret, a.aliceToken, p.secret, "whsec_critical_fixture", a.bob.ID} {
				require.False(t, strings.Contains(string(blob), private), "private fixture data appeared in shared assets/config")
			}
		}
		_, err := a.alpha.GetOrder(t.Context(), order.ID)
		require.NoError(t, err)
	})
}
