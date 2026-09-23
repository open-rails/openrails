package embed

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func reviewRuntime(cfg *HTTPConfig, delegated billingauth.DelegatedAuthenticator) *Runtime {
	if cfg != nil {
		for i := range cfg.CustomerRoutes {
			if cfg.CustomerRoutes[i].DelegatedAuthenticator == nil {
				cfg.CustomerRoutes[i].DelegatedAuthenticator = delegated
			}
		}
	}
	var auth *billingauth.Integration
	if cfg != nil && (cfg.Checkout || cfg.Catalog || cfg.MerchantAdmin || cfg.MerchantAPI || cfg.MerchantConfig) {
		auth = rejectingIntegration()
	}
	c := &config.Config{MerchantConfigHTTP: true, AllowCatalogUpdates: true}
	return &Runtime{httpConfig: cfg, delegatedAuthenticator: delegated, app: &app.App{Config: c, Runtime: &app.Runtime{Config: c, Auth: auth}}}
}
func reviewReject(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
	return nil, billingauth.ErrUnauthenticated
}
func reviewMount(t *testing.T, rt *Runtime) *http.ServeMux {
	t.Helper()
	routes, err := rt.HTTPRoutes()
	require.NoError(t, err)
	mux := http.NewServeMux()
	for _, route := range routes {
		mux.Handle(route.Method+" /api/pay"+route.Path, route.Handler)
	}
	return mux
}

func TestConfiguredRoutesReviewExposureAndCredentialOwnership(t *testing.T) {
	delegated := billingauth.DelegatedAuthenticatorFunc(reviewReject)
	rt := reviewRuntime(nil, delegated)
	routes, err := rt.HTTPRoutes()
	require.ErrorContains(t, err, "disabled")
	require.Empty(t, routes)
	rt = reviewRuntime(&HTTPConfig{}, delegated)
	routes, err = rt.HTTPRoutes()
	require.NoError(t, err)
	for _, r := range routes {
		require.NotContains(t, r.Path, "/catalog")
		require.NotContains(t, r.Path, "/me/")
		require.NotContains(t, r.Path, "/merchant/")
	}
	rt = reviewRuntime(&HTTPConfig{MerchantConfig: true}, delegated)
	rt.app.Config.SecretBackend = config.SecretBackendSnapshot
	routes, err = rt.HTTPRoutes()
	require.NoError(t, err)
	// Metadata routes remain mounted and authorized; custody is enforced when
	// a request actually supplies credentials.
	muxSnapshot := reviewMount(t, rt)
	wSnapshot := httptest.NewRecorder()
	muxSnapshot.ServeHTTP(wSnapshot, httptest.NewRequest(http.MethodPut, "/api/pay/v1/merchant/payment-providers/stripe", strings.NewReader(`{}`)))
	require.Equal(t, http.StatusUnauthorized, wSnapshot.Code)

	rt = reviewRuntime(&HTTPConfig{Checkout: true, CustomerRoutes: []CustomerRoutesConfig{{Treasury: true}}, MerchantAdmin: true, Catalog: true, MerchantConfig: true, MerchantAPI: true}, delegated)
	routes, err = rt.HTTPRoutes()
	require.NoError(t, err)
	require.NotEmpty(t, routes)
	mux := reviewMount(t, rt)
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/api/pay/v1/me/balance", http.StatusUnauthorized},
		{http.MethodPost, "/api/pay/v1/checkout", http.StatusUnauthorized},
		{http.MethodOptions, "/api/pay/v1/me/balance", http.StatusNoContent},
		{http.MethodOptions, "/api/pay/v1/checkout", http.StatusNoContent},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer invalid")
		mux.ServeHTTP(w, req)
		require.Equal(t, tc.code, w.Code, tc.path+" "+w.Body.String())
	}
}

func TestConfiguredRoutesReviewPreserveSignedRequest(t *testing.T) {
	const target = "/api/pay/v1/customers/a%2Fb/invoices/invoice-1?view=raw"
	const body = "{ \"signed\" : \"unaltered\" }\n"
	calls := 0
	delegated := billingauth.DelegatedAuthenticatorFunc(func(_ context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
		calls++
		require.Equal(t, target, r.RequestURI)
		require.Equal(t, "/api/pay/v1/customers/a/b/invoices/invoice-1", r.URL.Path)
		require.Equal(t, "/api/pay/v1/customers/a%2Fb/invoices/invoice-1", r.URL.RawPath)
		require.Equal(t, "a/b", r.PathValue("customer_id"))
		require.Equal(t, "invoice-1", r.PathValue("id"))
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, body, string(raw))
		return nil, billingauth.ErrUnauthenticated
	})
	mux := reviewMount(t, reviewRuntime(&HTTPConfig{CustomerRoutes: []CustomerRoutesConfig{{Treasury: true}}}, delegated))
	req := httptest.NewRequest(http.MethodGet, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer signed")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, 1, calls)
}

func TestConfiguredRoutesReviewSharedLimiterAtCustomPrefix(t *testing.T) {
	delegated := billingauth.DelegatedAuthenticatorFunc(reviewReject)
	rt := reviewRuntime(&HTTPConfig{CustomerRoutes: []CustomerRoutesConfig{{Treasury: true}}}, delegated)
	rt.app.Config.RateLimits = &config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}, "default": {RequestsPerMinute: 60}}
	mux := reviewMount(t, rt)
	for i, path := range []string{"/api/pay/v1/me/checkout", "/api/pay/v1/customers/customer-1/checkout"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.RemoteAddr = "203.0.113.94:1234"
		req.Header.Set("Authorization", "Bearer invalid")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if i == 0 {
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		} else {
			require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
		}
	}
}

func TestConfiguredRoutesReviewTrailingSlashPathValues(t *testing.T) {
	// Fiber's default router accepts the slash without redirecting. Binding must
	// preserve the original signed URL while extracting the same route values.
	handler := bindHTTPPathValues("/v1/customers/{customer_id}/invoices/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "acme", r.PathValue("customer_id"))
		require.Equal(t, "invoice-1", r.PathValue("id"))
		require.Equal(t, "/api/pay/v1/customers/acme/invoices/invoice-1/", r.URL.Path)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/pay/v1/customers/acme/invoices/invoice-1/", nil))
}

func TestConfiguredRoutesReviewInvalidAuthFailsBeforeDatabase(t *testing.T) {
	for _, tc := range []struct {
		cfg     HTTPConfig
		message string
	}{
		{HTTPConfig{Checkout: true}, "Checkout requires"},
		{HTTPConfig{CustomerRoutes: []CustomerRoutesConfig{{Treasury: true}}}, "requires its own authenticator"},
		{HTTPConfig{MerchantAdmin: true}, "management surfaces require"},
		{HTTPConfig{Catalog: true}, "management surfaces require"},
		{HTTPConfig{MerchantConfig: true}, "management surfaces require"},
		{HTTPConfig{MerchantAPI: true}, "management surfaces require"},
	} {
		_, err := New(context.Background(), Options{Config: &config.Config{}, HTTP: &tc.cfg})
		require.ErrorContains(t, err, tc.message)
	}
}

func rejectingIntegration() *billingauth.Integration {
	return &billingauth.Integration{Authentication: billingauth.AuthenticationFunc(func(context.Context, *http.Request) (billingauth.Identity, error) {
		return billingauth.Identity{}, billingauth.ErrUnauthenticated
	}), Authorization: billingauth.AuthorizationFunc(func(context.Context, *http.Request, billingauth.Identity, billingauth.Requirement) error {
		return billingauth.ErrUnauthenticated
	})}
}

func TestConfiguredRoutesOmitDisabledCatalogMutations(t *testing.T) {
	delegated := billingauth.DelegatedAuthenticatorFunc(reviewReject)
	rt := reviewRuntime(&HTTPConfig{Catalog: true, MerchantAdmin: true}, delegated)
	rt.app.Config.AllowCatalogUpdates = false
	routes, err := rt.HTTPRoutes()
	require.NoError(t, err)
	reads := 0
	for _, route := range routes {
		// Subscription repricing changes billing agreements, not catalog definitions.
		if strings.Contains(route.Path, "/catalog") && !strings.Contains(route.Path, "/catalog/reprice-") {
			require.Contains(t, []string{http.MethodGet, http.MethodHead, http.MethodOptions}, route.Method, route.Path)
			reads++
		}
	}
	require.Positive(t, reads)
	mux := reviewMount(t, rt)
	for _, path := range []string{"/api/pay/v1/merchant/catalog/products", "/api/pay/v1/catalog/products", "/api/pay/v1/merchant/catalogs"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, rec.Code, path)
	}
}
