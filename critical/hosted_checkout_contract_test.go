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
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

// declareCheckoutPage declares the payment page on a loopback origin of its
// own, from the built assets, before the engine is built;
// serveCheckoutPage serves it there.
func declareCheckoutPage(t *testing.T, cfg *openrails.Config, deps *openrails.Deps) *httptest.Server {
	t.Helper()
	assets := os.Getenv("OPENRAILS_CRITICAL_PAYMENT_ASSETS")
	require.NotEmpty(t, assets, "built real payment assets are mandatory")
	server := httptest.NewUnstartedServer(nil)
	t.Cleanup(server.Close)
	cfg.CheckoutOrigin = "http://" + server.Listener.Addr().String()
	deps.CheckoutAssets = os.DirFS(assets)
	return server
}

func serveCheckoutPage(t *testing.T, server *httptest.Server, engine *openrails.Engine) *httptest.Server {
	t.Helper()
	origin, handler := engine.CheckoutPage()
	require.Equal(t, "http://"+server.Listener.Addr().String(), origin)
	require.NotNil(t, handler)
	server.Config.Handler = handler
	server.Start()
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
func hostPayInvoice(t *testing.T, a *authorityWorld, key string, embedded bool) (*billing.Invoice, string) {
	t.Helper()
	price := hostProduct(t, a.alpha, key)
	invoice, err := a.alpha.CreateInvoice(t.Context(), key, billing.CreateInvoiceParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, PayURL: &billing.InvoicePayURLParams{SuccessURL: alphaSite + "/done?invoice={INVOICE_ID}", Embedded: embedded}})
	require.NoError(t, err)
	require.NotNil(t, invoice.PayURL)
	address, err := url.Parse(*invoice.PayURL)
	require.NoError(t, err)
	require.NotEmpty(t, address.Fragment)
	return invoice, address.Fragment
}
func TestHostedCheckout(t *testing.T) {
	t.Run("OR-J03-S01/capability-pays-only-its-invoice", func(t *testing.T) {
		var page *httptest.Server
		a, p := newPaymentWorld(t, func(w *world) { page = declareCheckoutPage(t, &w.cfg, &w.deps) })
		serveCheckoutPage(t, page, a.engine)
		invoice, secret := hostPayInvoice(t, a, "hosted", false)
		status, headers, raw := hostPageRequest(t, page, "GET", "/v1/me/invoices/"+invoice.ID.String(), secret, nil)
		require.Equal(t, 200, status, string(raw))
		require.Equal(t, "no-store", headers.Get("Cache-Control"))
		var found billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &found))
		require.Equal(t, invoice.ID, found.ID)
		require.EqualValues(t, 10_000_000, found.Total)
		status, _, raw = hostPageRequest(t, page, "GET", "/v1/me/config", secret, nil)
		require.Equal(t, 200, status, string(raw))
		status, _, raw = hostPageRequest(t, page, "GET", "/v1/me/payment-methods", secret, nil)
		require.Equal(t, 404, status, string(raw))
		token := p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})
		status, _, raw = hostPageRequest(t, page, "POST", "/v1/me/invoices/"+invoice.ID.String()+"/pay", secret, billing.PayInvoiceParams{Payment: billing.InvoicePaymentParams{Token: token}, ExpectedTotal: 10_000_000}, map[string]string{"Idempotency-Key": "hosted-pay"})
		require.Equal(t, 200, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &found))
		require.Equal(t, billing.InvoiceComplete, found.Status)
		require.Len(t, p.effects(t), 1)
		require.EqualValues(t, 1000, p.effects(t)[0].Amount)
		canonical, err := a.alpha.GetInvoice(t.Context(), invoice.ID)
		require.NoError(t, err)
		require.Equal(t, found.Payment.PaymentID, canonical.Payment.PaymentID)
	})
	t.Run("OR-J03-S02/foreign-tampered-and-expired-capability-refused", func(t *testing.T) {
		var page *httptest.Server
		a, p := newPaymentWorld(t, func(w *world) { page = declareCheckoutPage(t, &w.cfg, &w.deps) })
		serveCheckoutPage(t, page, a.engine)
		first, secret := hostPayInvoice(t, a, "first", false)
		second, secondSecret := hostPayInvoice(t, a, "second", false)
		for _, trial := range []struct {
			id    billing.InvoiceID
			token string
		}{{first.ID, secondSecret}, {second.ID, secret}, {first.ID, secret[:len(secret)-2] + "XX"}, {first.ID, ""}} {
			status, _, raw := hostPageRequest(t, page, "GET", "/v1/me/invoices/"+trial.id.String(), trial.token, nil)
			require.Equal(t, 404, status, string(raw))
		}
		a.clock.Advance(first.PayURLExpiresAt.Sub(a.clock.Now()))
		status, _, raw := hostPageRequest(t, page, "GET", "/v1/me/invoices/"+first.ID.String(), secret, nil)
		require.Equal(t, 410, status, string(raw))
		require.Contains(t, string(raw), "pay_url_expired")
		require.Empty(t, p.effects(t))
		money, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID))})
		require.NoError(t, err)
		require.Empty(t, money.Items)
	})
	t.Run("OR-J03-S03/checkout-and-api-authority-stay-separate", func(t *testing.T) {
		var page *httptest.Server
		a, _ := newPaymentWorld(t, func(w *world) {
			declaration := cfgMerchant(w.cfg, "alpha")
			declaration.Checkout = &billing.MerchantCheckoutSettings{EmbedOrigins: []string{"https://merchant.critical.invalid"}}
			setCfgMerchant(&w.cfg, "alpha", declaration)
			page = declareCheckoutPage(t, &w.cfg, &w.deps)
		})
		serveCheckoutPage(t, page, a.engine)
		invoice, secret := hostPayInvoice(t, a, "embedded", true)
		for _, token := range []string{secret, a.alphaKey.Secret, a.aliceToken} {
			status, _, raw := hostPageRequest(t, page, "GET", "/v1/admin/customers", token, nil)
			require.Equal(t, 404, status, string(raw))
		}
		status, raw := a.request(t, "GET", "/v1/admin/customers", secret, nil)
		require.Equal(t, 401, status, string(raw))
		status, headers, raw := hostPageRequest(t, page, "GET", "/pay/"+invoice.ID.String(), "", nil, map[string]string{"Origin": "https://evil.critical.invalid"})
		require.Equal(t, 200, status, string(raw))
		csp := headers.Get("Content-Security-Policy")
		require.Contains(t, csp, "frame-ancestors https://merchant.critical.invalid")
		require.NotContains(t, csp, "evil.critical.invalid")
		require.Empty(t, headers.Get("Access-Control-Allow-Origin"))
	})
	t.Run("OR-J03-S06/return-urls-name-only-the-merchants-own-sites", func(t *testing.T) {
		const alphaShop, betaShop = "https://shop.alpha.critical.invalid", "https://shop.beta.critical.invalid"
		a, p := newPaymentWorld(t, func(w *world) {
			for slug, site := range map[string]string{"alpha": alphaShop, "beta": betaShop} {
				declaration := cfgMerchant(w.cfg, slug)
				declaration.Checkout = &billing.MerchantCheckoutSettings{EmbedOrigins: []string{site}}
				setCfgMerchant(&w.cfg, slug, declaration)
			}
			declareCheckoutPage(t, &w.cfg, &w.deps)
		})
		price := hostProduct(t, a.alpha, "returns")
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		payURL := func(key, success string, cancel *string) error {
			_, err := a.alphaRemote.CreateInvoice(t.Context(), key, billing.CreateInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: price.ID}},
				PayURL: &billing.InvoicePayURLParams{SuccessURL: success, CancelURL: cancel}})
			return err
		}
		refused := func(err error, param string) {
			t.Helper()
			var apiErr *billing.StatusError
			require.ErrorAs(t, err, &apiErr)
			require.Equal(t, http.StatusBadRequest, apiErr.Status)
			require.Equal(t, billing.CodeInvalidParam, apiErr.Code)
			require.Equal(t, ptr(param), apiErr.Param)
		}
		refused(payURL("beta-success", betaShop+"/done", nil), "pay_url.success_url")
		refused(payURL("beta-cancel", alphaShop+"/done", ptr(betaShop+"/cart")), "pay_url.cancel_url")
		refused(payURL("unlisted", "https://critical.invalid/done", nil), "pay_url.success_url")
		require.NoError(t, payURL("issuer", alphaSite+"/done?invoice={INVOICE_ID}", ptr(alphaSite+"/cart")), "the merchant's issuer origin is its own")
		require.NoError(t, payURL("embed", alphaShop+"/done?invoice={INVOICE_ID}", ptr(alphaShop+"/cart")), "a registered embed origin is its own")

		sale := saleInput(t, a, p)
		hosted := func(key, returnURL string) (int, []byte) {
			return a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, billing.CreateMyInvoiceParams{Lines: sale.Lines, ExpectedTotal: sale.ExpectedTotal,
				Payment: &billing.InvoicePaymentParams{ReturnURL: returnURL}}, map[string]string{"Idempotency-Key": key})
		}
		status, raw := hosted("hosted-beta", betaShop+"/paid")
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		require.Contains(t, string(raw), `"code":"invalid_param"`)
		status, raw = hosted("hosted-embed", alphaShop+"/paid")
		require.Equal(t, http.StatusCreated, status, string(raw))
		var invoice billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &invoice))
		require.NotNil(t, invoice.Payment.NextAction)
		require.Equal(t, "redirect_to_url", invoice.Payment.NextAction.Type)
	})
	t.Run("OR-J03-S05/real-assets-and-config-redact-private-state", func(t *testing.T) {
		var page *httptest.Server
		a, p := newPaymentWorld(t, func(w *world) { page = declareCheckoutPage(t, &w.cfg, &w.deps) })
		serveCheckoutPage(t, page, a.engine)
		invoice, secret := hostPayInvoice(t, a, "assets", false)
		status, headers, html := hostPageRequest(t, page, "GET", "/pay/"+invoice.ID.String(), "", nil)
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
		_, err := a.alpha.GetInvoice(t.Context(), invoice.ID)
		require.NoError(t, err)
	})
}
