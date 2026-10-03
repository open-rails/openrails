package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/routebundle"
)

// httpRuntime is a database-free runtime: route materialization and every
// refusal exercised here happen before a handler touches storage.
func httpRuntime(cfg *config.HTTPConfig, managementAuth bool) *Engine {
	var auth *billingauth.Integration
	if managementAuth {
		reject := func(context.Context, *http.Request) (billingauth.Identity, error) {
			return billingauth.Identity{}, billingauth.ErrUnauthenticated
		}
		auth = &billingauth.Integration{
			Authentication: billingauth.AuthenticationFunc(reject),
			Authorization: billingauth.AuthorizationFunc(func(context.Context, *http.Request, billingauth.Identity, billingauth.Requirement) error {
				return billingauth.ErrUnauthenticated
			}),
		}
	}
	c := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, MerchantConfigHTTP: true, AllowCatalogUpdates: true}
	return &Engine{http: cfg, App: &app.App{Config: c, Runtime: &app.Runtime{Config: c, Auth: auth}}}
}

func rejectDelegated(*http.Request) (*billingauth.DelegatedPrincipal, error) {
	return nil, billingauth.ErrUnauthenticated
}

func mountAt(t *testing.T, rt *Engine, prefix string) *http.ServeMux {
	t.Helper()
	routes, err := rt.Routes()
	require.NoError(t, err)
	mux := http.NewServeMux()
	for _, route := range routes {
		mux.Handle(route.Method+" "+prefix+route.Path, route.Handler)
	}
	return mux
}

func serve(h http.Handler, method, target, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Config.HTTP is copied at construction and Routes materializes it once:
// neither the host's later edits nor a caller's edits to returned routes leak.
func TestHTTPConfigurationIsCopiedAndRoutesMemoized(t *testing.T) {
	_, err := httpRuntime(nil, false).Routes()
	require.ErrorContains(t, err, "HTTP is disabled")

	sandbox := config.Config{TestMode: config.CredentialPostureSandbox}
	sandbox.HTTP = &config.HTTPConfig{CustomerRoutes: []config.CustomerRoutesConfig{{Treasury: true}}}
	_, err = httpConfig(sandbox, nil)
	require.ErrorContains(t, err, "requires")
	policy := &config.HTTPConfig{CustomerRoutes: []config.CustomerRoutesConfig{{Prefix: "/portal", Authenticate: rejectDelegated}}}
	sandbox.HTTP = policy
	copied, err := httpConfig(sandbox, nil)
	require.NoError(t, err)
	policy.Catalog = true
	policy.CustomerRoutes[0].Prefix = "/mutated"

	rt := httpRuntime(copied, false)
	routes, err := rt.Routes()
	require.NoError(t, err)
	var portal bool
	for _, route := range routes {
		require.NotContains(t, route.Path, "catalog")
		require.NotContains(t, route.Path, "/mutated")
		portal = portal || strings.HasPrefix(route.Path, "/portal/")
	}
	require.True(t, portal)
	original := routes[0].Path
	routes[0].Path = "/caller-mutated"
	again, err := rt.Routes()
	require.NoError(t, err)
	require.Equal(t, original, again[0].Path)

	concurrent := httpRuntime(&config.HTTPConfig{}, false)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { _, err := concurrent.Routes(); require.NoError(t, err) })
	}
	wg.Wait()
}

func TestHTTPRouteExposureMatchesConfiguration(t *testing.T) {
	routes, err := httpRuntime(&config.HTTPConfig{}, false).Routes()
	require.NoError(t, err)
	require.NotEmpty(t, routes)
	for _, r := range routes {
		for _, hidden := range []string{"/catalog", "/me/", "/merchant/"} {
			require.NotContains(t, r.Path, hidden, "an empty policy exposes no customer or management surface")
		}
	}

	full := &config.HTTPConfig{Checkout: true, MerchantAdmin: true, Catalog: true, MerchantConfig: true, MerchantAPI: true,
		CustomerRoutes: []config.CustomerRoutesConfig{{Treasury: true, Authenticate: rejectDelegated}}}
	rt := httpRuntime(full, true)
	rt.App.Config.SecretBackend = config.SecretBackendSnapshot
	mux := mountAt(t, rt, "/api/pay")
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/api/pay/v1/me/balance", http.StatusUnauthorized},
		{http.MethodPost, "/api/pay/v1/checkout", http.StatusUnauthorized},
		{http.MethodPut, "/api/pay/v1/merchant/payment-providers/stripe", http.StatusUnauthorized},
		{http.MethodOptions, "/api/pay/v1/me/balance", http.StatusNoContent},
		{http.MethodOptions, "/api/pay/v1/checkout", http.StatusNoContent},
	} {
		rec := serve(mux, tc.method, tc.path, "{}", "Authorization", "Bearer invalid")
		require.Equal(t, tc.code, rec.Code, tc.method+" "+tc.path+" "+rec.Body.String())
	}
}

func TestHTTPCatalogMutationsOmittedWhenDisabled(t *testing.T) {
	rt := httpRuntime(&config.HTTPConfig{Catalog: true, MerchantAdmin: true}, true)
	rt.App.Config.AllowCatalogUpdates = false
	routes, err := rt.Routes()
	require.NoError(t, err)
	reads := 0
	for _, route := range routes {
		// Repricing changes agreements, not definitions; offer lookup is a read with a POST body.
		if strings.Contains(route.Path, "/catalog") && !strings.Contains(route.Path, "/catalog/reprice-") && !strings.HasSuffix(route.Path, "/offers/lookup") {
			require.Contains(t, []string{http.MethodGet, http.MethodHead, http.MethodOptions}, route.Method, route.Path)
			reads++
		}
	}
	require.Positive(t, reads)
	mux := mountAt(t, rt, "/api/pay")
	for _, path := range []string{"/api/pay/v1/merchant/catalog/products", "/api/pay/v1/catalog/products", "/api/pay/v1/merchant/catalogs"} {
		require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, serve(mux, http.MethodPost, path, "").Code, path)
	}
}

// Delegated verifiers may check signatures over the exact request, so the
// adapter must hand them the original URI, raw path and unread body.
func TestHTTPVerifierSeesOriginalSignedRequest(t *testing.T) {
	const target = "/api/pay/v1/customers/a%2Fb/invoices/invoice-1?view=raw"
	const body = "{ \"signed\" : \"unaltered\" }\n"
	calls := 0
	verifier := func(r *http.Request) (*billingauth.DelegatedPrincipal, error) {
		calls++
		require.Equal(t, target, r.RequestURI)
		require.Equal(t, "/api/pay/v1/customers/a%2Fb/invoices/invoice-1", r.URL.RawPath)
		require.Equal(t, "a/b", r.PathValue("customer_id"))
		require.Equal(t, "invoice-1", r.PathValue("id"))
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, body, string(raw))
		return nil, billingauth.ErrUnauthenticated
	}
	mux := mountAt(t, httpRuntime(&config.HTTPConfig{CustomerRoutes: []config.CustomerRoutesConfig{{Treasury: true, Authenticate: verifier}}}, false), "/api/pay")
	require.Equal(t, http.StatusUnauthorized, serve(mux, http.MethodGet, target, body, "Authorization", "Bearer signed").Code)
	require.Equal(t, 1, calls)

	// Routers that accept a trailing slash without redirecting still bind values.
	bound := routebundle.BindPathValues("/v1/customers/{customer_id}/invoices/{id}", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		require.Equal(t, "acme", r.PathValue("customer_id"))
		require.Equal(t, "invoice-1", r.PathValue("id"))
		require.Equal(t, "/api/pay/v1/customers/acme/invoices/invoice-1/", r.URL.Path)
		calls++
	}))
	serve(bound, http.MethodGet, "/api/pay/v1/customers/acme/invoices/invoice-1/", "")
	require.Equal(t, 2, calls)
}

// One limiter per runtime: a second host mount or a sibling route in the same
// bucket must not reset the counters.
func TestHTTPRateLimitIsSharedAcrossMountsAndRoutes(t *testing.T) {
	rt := httpRuntime(&config.HTTPConfig{CustomerRoutes: []config.CustomerRoutesConfig{{Treasury: true, Authenticate: rejectDelegated}}}, false)
	rt.App.Config.RateLimits = &config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}, "default": {RequestsPerMinute: 60}}
	first, second := mountAt(t, rt, "/first"), mountAt(t, rt, "/second")
	for i, tc := range []struct {
		mux  http.Handler
		path string
	}{
		{first, "/first/v1/me/checkout"},
		{second, "/second/v1/me/checkout"},
		{first, "/first/v1/customers/customer-1/checkout"},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("{}"))
		req.RemoteAddr = "203.0.113.94:1234"
		req.Header.Set("Authorization", "Bearer invalid")
		rec := httptest.NewRecorder()
		tc.mux.ServeHTTP(rec, req)
		want := http.StatusTooManyRequests
		if i == 0 {
			want = http.StatusUnauthorized
		}
		require.Equal(t, want, rec.Code, tc.path)
	}
}

func TestCustomerExposuresKeepTheirOwnAuthority(t *testing.T) {
	calls := map[string]int{}
	verifier := func(audience string) func(*http.Request) (*billingauth.DelegatedPrincipal, error) {
		return func(r *http.Request) (*billingauth.DelegatedPrincipal, error) {
			calls[audience]++
			require.Contains(t, r.RequestURI, "?proof=original")
			if audience == "platform" {
				require.Equal(t, "host-selected", r.PathValue("slug"))
			}
			if r.Header.Get("Authorization") != "Bearer "+audience {
				return nil, billingauth.ErrUnauthenticated
			}
			return &billingauth.DelegatedPrincipal{MerchantID: "11111111-1111-4111-8111-111111111111", SubjectID: "22222222-2222-4222-8222-222222222222"}, nil
		}
	}
	rt := httpRuntime(&config.HTTPConfig{CustomerRoutes: []config.CustomerRoutesConfig{
		{Prefix: "/billing/v1/me", Authenticate: verifier("portal")},
		{Prefix: "/api/v1/merchants/{slug}/billing/me", Scope: config.CustomerSubscriptionManagement, Authenticate: verifier("platform")},
	}}, false)
	mux := mountAt(t, rt, "/api/pay")
	const portal, platform = "/billing/v1/me", "/api/v1/merchants/host-selected/billing/me"
	for _, tc := range []struct {
		prefix, token string
		status        int
	}{
		{portal, "portal", 400},
		{portal, "platform", 401},
		{platform, "portal", 401},
		{platform, "platform", 400},
	} {
		rec := serve(mux, http.MethodPost, "/api/pay"+tc.prefix+"/subscriptions/not-id/cancel?proof=original", "{}", "Authorization", "Bearer "+tc.token)
		require.Equal(t, tc.status, rec.Code, rec.Body.String())
	}
	require.Equal(t, map[string]int{"portal": 2, "platform": 2}, calls)
	for _, path := range []string{portal + "/merchant/customers", "/billing/v1/customers/x/balance", "/billing/v1/merchant/payment-providers", platform + "/checkout", platform + "/payment-methods"} {
		require.Equal(t, http.StatusNotFound, serve(mux, http.MethodPost, "/api/pay"+path, "").Code, path)
	}
	routes, err := rt.Routes()
	require.NoError(t, err)
	actions := 0
	for _, route := range routes {
		if strings.HasPrefix(route.Path, "/api/v1/merchants/") && route.Method != http.MethodOptions {
			actions++
		}
	}
	require.Equal(t, 4, actions, "subscription management exposes exactly its four actions")
}

func TestCustomerExposureValidation(t *testing.T) {
	validate := func(routes ...config.CustomerRoutesConfig) error {
		_, err := httpConfig(config.Config{HTTP: &config.HTTPConfig{CustomerRoutes: routes}}, nil)
		return err
	}
	for _, prefix := range []string{"/", "/customer/", "/customer/../other", "/customer/{tail...}", "/customer/{slug}/%2f"} {
		require.Error(t, validate(config.CustomerRoutesConfig{Prefix: prefix, Authenticate: rejectDelegated}), prefix)
	}
	require.ErrorContains(t, validate(config.CustomerRoutesConfig{Prefix: "/portal"}), "own authenticator",
		"without Deps.Authenticate a customer mount needs its own")
	conflicting, err := httpConfig(config.Config{HTTP: &config.HTTPConfig{CustomerRoutes: []config.CustomerRoutesConfig{
		{Prefix: "/v1/me", Authenticate: rejectDelegated}, {Prefix: "/v1/me", Authenticate: rejectDelegated},
	}}}, nil)
	require.NoError(t, err)
	_, err = httpRuntime(conflicting, false).Routes()
	require.ErrorContains(t, err, "conflicting")
}

func TestCustomerBillingManagementScope(t *testing.T) {
	rt := httpRuntime(&config.HTTPConfig{CustomerRoutes: []config.CustomerRoutesConfig{{Prefix: "/v1/me", Scope: config.CustomerBillingManagement, Authenticate: rejectDelegated}}}, false)
	mux := mountAt(t, rt, "/api/pay")
	for _, path := range []string{"/products", "/payments", "/invoices", "/subscriptions", "/payment-methods", "/checkout/cs_existing"} {
		require.Equal(t, http.StatusUnauthorized, serve(mux, http.MethodGet, "/api/pay/v1/me"+path, "").Code, path)
	}
	require.Equal(t, http.StatusUnauthorized, serve(mux, http.MethodPost, "/api/pay/v1/me/checkout/cs_existing/confirm", "").Code)
	for _, path := range []string{"/checkout", "/subscriptions/x/change-tier", "/subscriptions/x/provider-cutover", "/billing-portal", "/subscriptions/x/solana-tier-change"} {
		require.Equal(t, http.StatusNotFound, serve(mux, http.MethodPost, "/api/pay/v1/me"+path, "").Code, path)
	}
	rec := serve(mux, http.MethodGet, "/api/pay/v1/capabilities", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var capabilities struct {
		RouteGroups map[string]bool `json:"route_groups"`
		Features    map[string]bool `json:"features"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &capabilities))
	require.True(t, capabilities.RouteGroups["customer"])
	require.False(t, capabilities.RouteGroups["checkout"])
	for _, feature := range []string{"stripe_billing_portal", "solana_one_time_payments", "provider_credential_writes"} {
		require.False(t, capabilities.Features[feature], feature)
	}
	require.NotContains(t, capabilities.Features, "webhooks")
	require.NotContains(t, rec.Body.String(), `"routes"`)
}

// Customer mounts ignore ambient cookies unless HTTP.CookieOrigin admits
// them, and admission still requires that origin.
func TestCustomerCookieAdmission(t *testing.T) {
	calls := 0
	authn := func(r *http.Request) (*billingauth.DelegatedPrincipal, error) {
		calls++
		if _, err := r.Cookie("session"); err != nil {
			return nil, billingauth.ErrUnauthenticated
		}
		return &billingauth.DelegatedPrincipal{MerchantID: "11111111-1111-4111-8111-111111111111", SubjectID: "22222222-2222-4222-8222-222222222222"}, nil
	}
	routes := []config.CustomerRoutesConfig{{Prefix: "/portal", Scope: config.CustomerSubscriptionManagement, Authenticate: authn}}
	mux := mountAt(t, httpRuntime(&config.HTTPConfig{CustomerRoutes: routes}, false), "/api/pay")
	admitting := mountAt(t, httpRuntime(&config.HTTPConfig{CustomerRoutes: routes, CookieOrigin: "https://portal.example"}, false), "/api/pay")
	_, err := httpConfig(config.Config{HTTP: &config.HTTPConfig{CookieOrigin: "http://portal.example"}}, nil)
	require.ErrorContains(t, err, "CookieOrigin", "plain HTTP only on loopback")
	for _, tc := range []struct {
		name, origin, bearer string
		admitted             bool
		code, authCalls      int
	}{
		{name: "ambient cookie stripped", code: 401, authCalls: 1},
		{name: "missing origin", admitted: true, code: 403},
		{name: "foreign origin", origin: "https://other.example", admitted: true, code: 403},
		{name: "admitted session", origin: "https://portal.example", admitted: true, code: 400, authCalls: 1},
		{name: "explicit credential cannot fall back", origin: "https://portal.example", bearer: "Bearer invalid", admitted: true, code: 401, authCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls = 0
			req := httptest.NewRequest(http.MethodPost, "/api/pay/portal/subscriptions/not-id/cancel", strings.NewReader("{}"))
			req.AddCookie(&http.Cookie{Name: "session", Value: "valid"})
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.bearer != "" {
				req.Header.Set("Authorization", tc.bearer)
			}
			var target http.Handler = mux
			if tc.admitted {
				target = admitting
			}
			rec := httptest.NewRecorder()
			target.ServeHTTP(rec, req)
			require.Equal(t, tc.code, rec.Code, rec.Body.String())
			require.Equal(t, tc.authCalls, calls)
		})
	}
}
