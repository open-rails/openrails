package openrailshttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/routebundle"
)

// Bundle stands in for a Client's routes in these unit tests.
type Bundle struct {
	routes   []openrails.Route
	rootOnly bool
}

func Routes(s routeSource) (*Bundle, error) { return &Bundle{routes: s.routes, rootOnly: s.root}, nil }

func toRoutes(in []routebundle.Route) []openrails.Route {
	out := make([]openrails.Route, len(in))
	for i, r := range in {
		out[i] = openrails.Route{Method: r.Method, Path: r.Path, Handler: r.Handler}
	}
	return out
}

func (b *Bundle) Mount(target any, prefix ...string) error {
	if b == nil {
		return errors.New("nil bundle")
	}
	return mount(target, b.routes, b.rootOnly, false, prefix...)
}

func (b *Bundle) MountRoot(target any) error { return mount(target, b.routes, b.rootOnly, true) }

type routeSource struct {
	routes []openrails.Route
	root   bool
}

// inventoryBundle builds every configured route family the way the engine does.
func inventoryBundle(t *testing.T) *Bundle {
	t.Helper()
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, AllowCatalogUpdates: true, SecretBackend: config.SecretBackendDB}
	deny := func(*http.Request, string) (*billingauth.DelegatedPrincipal, error) {
		return nil, billingauth.ErrUnauthenticated
	}
	auth := &billingauth.Integration{
		Authentication: billingauth.AuthenticationFunc(func(context.Context, *http.Request) (billingauth.Identity, error) {
			return billingauth.Identity{}, billingauth.ErrUnauthenticated
		}),
		Authorization: billingauth.AuthorizationFunc(func(context.Context, *http.Request, billingauth.Identity, billingauth.Requirement) error {
			return billingauth.ErrUnauthenticated
		}),
	}
	graph := &app.App{Config: cfg, Runtime: &app.Runtime{Config: cfg, Auth: auth, AuthenticateCustomer: deny}}
	policy := &config.HTTPConfig{Checkout: &config.CheckoutConfig{}, CustomerRoutes: []config.CustomerRoutesConfig{{Delegated: true}},
		Merchant: true}
	table, err := embedhttp.ConfiguredRoutes(graph, policy)
	require.NoError(t, err)
	for i := range table.Entries {
		table.Entries[i].Path = strings.TrimPrefix(table.Entries[i].Path, "/billing")
	}
	require.NoError(t, embedhttp.ValidateRouteTable(table))
	bundle, err := Routes(routeSource{routes: toRoutes(routebundle.FromTable(table))})
	require.NoError(t, err)
	return bundle
}

func serve(target http.Handler, method, path string, body io.Reader) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer invalid")
	target.ServeHTTP(w, req)
	return w
}

// mountings are the supported host shapes: ServeMux with a prefix and a Chi group.
func mountings(t *testing.T, b *Bundle) map[string]http.Handler {
	mux := http.NewServeMux()
	require.NoError(t, b.Mount(mux, "/api/pay/"))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	router := chi.NewRouter()
	router.Route("/api/pay", func(group chi.Router) { require.NoError(t, b.Mount(group)) })
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
			{http.MethodPost, "/api/pay/v1/merchant/entitlements/lookup", http.StatusUnauthorized},
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
	b := &Bundle{routes: []openrails.Route{{Method: http.MethodPost, Path: "/v1/webhooks/{provider}/{account_id}", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// Anchored standalone routes refuse prefixes and subrouters before registering anything.
func TestRootOnlyBundleRequiresTheRootRouter(t *testing.T) {
	b, err := Routes(routeSource{root: true, routes: []openrails.Route{{Method: http.MethodGet, Path: "/admin/{asset...}", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/admin/js/site.js?q=raw", r.RequestURI)
		w.WriteHeader(http.StatusNoContent)
	})}}})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.ErrorContains(t, b.Mount(mux, "/outer"), "root")
	require.Equal(t, http.StatusNotFound, serve(mux, http.MethodGet, "/outer/admin/a", nil).Code)
	require.NoError(t, b.Mount(mux))
	router := chi.NewRouter()
	require.ErrorContains(t, b.Mount(router), "MountRoot")
	require.Empty(t, router.Routes())
	require.NoError(t, b.MountRoot(router))
	for _, target := range []http.Handler{mux, router} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			require.Equal(t, http.StatusNoContent, serve(target, method, "/admin/js/site.js?q=raw", nil).Code)
		}
	}
}

func TestMountRefusesInvalidTargets(t *testing.T) {
	b := &Bundle{}
	var nilBundle *Bundle
	require.Error(t, nilBundle.Mount(http.NewServeMux()))
	require.Error(t, b.Mount(struct{}{}))
	require.Error(t, b.Mount(http.NewServeMux(), "/a", "/b"))
	for _, prefix := range []string{"api", "/api/{id}", "/api?x", "/a b", "/a#b"} {
		require.Error(t, b.Mount(http.NewServeMux(), prefix), prefix)
	}
}

// A client that serves no payment page lets only itself frame one.
func TestCheckoutFramePolicy(t *testing.T) {
	page := CheckoutFramePolicy(nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "page") }))
	w := httptest.NewRecorder()
	page.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/checkout", nil))
	require.Equal(t, "frame-ancestors 'self'", w.Header().Get("Content-Security-Policy"))
	require.Equal(t, "page", w.Body.String())
}
