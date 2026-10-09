package openrailshttp

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth/authtest"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/routebundle"
)

// Bundle stands in for a Client's routes in these unit tests.
type Bundle struct{ routes []openrails.Route }

func toRoutes(in []routebundle.Route) []openrails.Route {
	out := make([]openrails.Route, len(in))
	for i, r := range in {
		out[i] = openrails.Route{Method: r.Method, Path: r.Path, Handler: r.Handler}
	}
	return out
}

func (b *Bundle) Mount(target any) error {
	if b == nil {
		return errors.New("nil bundle")
	}
	return mount(target, b.routes)
}

// inventoryBundle builds every configured route family the way the engine does.
func inventoryBundle(t *testing.T) *Bundle {
	t.Helper()
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, SecretBackend: config.SecretBackendDB}
	rt := &app.Runtime{Config: cfg}
	rt.SetConfiguredMerchant(testMerchant)
	graph := &app.App{Config: cfg, Runtime: rt}
	selection := config.Routes{Auth: authtest.Deny{}, Storefront: true, Merchant: true, CatalogEdits: true,
		CustomerProfiles: []config.CustomerRoutes{{Scope: config.CustomerSelfService}}}
	table, err := embedhttp.ConfiguredRoutes(graph, selection)
	require.NoError(t, err)
	for i := range table.Entries {
		table.Entries[i].Path = "/api/pay" + strings.TrimPrefix(table.Entries[i].Path, "/billing")
	}
	require.NoError(t, embedhttp.ValidateRouteTable(table))
	return &Bundle{routes: toRoutes(routebundle.FromTable(table))}
}

func serve(target http.Handler, method, path string, body io.Reader) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer invalid")
	target.ServeHTTP(w, req)
	return w
}

// mountings are the supported root routers: ServeMux and Chi.
func mountings(t *testing.T, b *Bundle) map[string]http.Handler {
	mux := http.NewServeMux()
	require.NoError(t, b.Mount(mux))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	router := chi.NewRouter()
	require.NoError(t, b.Mount(router))
	router.NotFound(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	router.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	return map[string]http.Handler{"servemux": mux, "chi": router}
}

func TestInventoryMountsNativelyUnderAPrefix(t *testing.T) {
	for name, engine := range mountings(t, inventoryBundle(t)) {
		for _, tc := range []struct {
			method, path string
			code         int
		}{
			{http.MethodGet, "/api/pay/v1/capabilities", http.StatusOK},
			{http.MethodHead, "/api/pay/v1/capabilities", http.StatusOK},
			{http.MethodPost, "/api/pay/v1/merchant/product-access", http.StatusUnauthorized},
			{http.MethodPost, "/api/pay/v1/merchant/customers/entitlementsXbatch", http.StatusTeapot},
			{http.MethodOptions, "/api/pay/v1/checkout-sessions/ocs_x/pay", http.StatusNoContent},
			{http.MethodGet, "/api/pay/v1/unrelated", http.StatusTeapot},
			{http.MethodGet, "/api/payment/v1/capabilities", http.StatusTeapot},
			{http.MethodGet, "/v1/capabilities", http.StatusTeapot},
		} {
			w := serve(engine, tc.method, tc.path, nil)
			require.Equal(t, tc.code, w.Code, "%s %s %s: %s", name, tc.method, tc.path, w.Body.String())
		}
	}
}

// Signed webhooks need the exact bytes, URI and headers the provider sent.
func TestWebhookRequestReachesHandlerUnchanged(t *testing.T) {
	const body = "{ \"signed\" : \"bytes\", \"unicode\": \"é\" }\n"
	const target = "/api/pay/v1/webhooks/stripe/acct_test?signature=unchanged"
	calls := 0
	b := &Bundle{routes: []openrails.Route{{Method: http.MethodPost, Path: "/api/pay/v1/webhooks/{provider}/{account_id}", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, body, string(raw))
		require.Equal(t, target, r.RequestURI)
		require.Equal(t, "signed-header", r.Header.Get("Stripe-Signature"))
		w.WriteHeader(http.StatusNoContent)
	})}}}
	for name, engine := range mountings(t, b) {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
		req.Header.Set("Stripe-Signature", "signed-header")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		require.Equal(t, http.StatusNoContent, w.Code, name)
		require.Equal(t, http.StatusTeapot, serve(engine, http.MethodGet, target, nil).Code, name+": only the declared method")
	}
	require.Equal(t, 2, calls)
}

// A subtree route (the admin console's assets) mounts at the root of either
// router, for GET and HEAD.
func TestSubtreeRouteMountsAtTheRoot(t *testing.T) {
	b := &Bundle{routes: []openrails.Route{{Method: http.MethodGet, Path: "/admin/{asset...}", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/admin/js/site.js?q=raw", r.RequestURI)
		w.WriteHeader(http.StatusNoContent)
	})}}}
	mux := http.NewServeMux()
	require.NoError(t, b.Mount(mux))
	router := chi.NewRouter()
	require.NoError(t, b.Mount(router))
	for _, target := range []http.Handler{mux, router} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			require.Equal(t, http.StatusNoContent, serve(target, method, "/admin/js/site.js?q=raw", nil).Code)
		}
	}
}

func TestMountRefusesInvalidTargets(t *testing.T) {
	var nilBundle *Bundle
	require.Error(t, nilBundle.Mount(http.NewServeMux()))
	require.Error(t, (&Bundle{}).Mount(struct{}{}))
	require.ErrorContains(t, Mount(http.NewServeMux(), nil, openrails.Routes{}), "requires a client")
}

var testMerchant = billing.MerchantID(uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
