package embed

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

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// httpRuntime is a database-free runtime: route materialization and every
// refusal exercised here happen before a handler touches storage.
func httpRuntime(cfg *HTTPConfig, managementAuth bool) *Runtime {
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
	return &Runtime{httpConfig: cfg, app: &app.App{Config: c, Runtime: &app.Runtime{Config: c, Auth: auth}}}
}

var rejectDelegated = billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
	return nil, billingauth.ErrUnauthenticated
})

func mountAt(t *testing.T, rt *Runtime, prefix string) *http.ServeMux {
	t.Helper()
	routes, err := rt.HTTPRoutes()
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

func TestHTTPConfigurationIsOneShotCopiedAndFrozen(t *testing.T) {
	disabled := httpRuntime(nil, false)
	_, err := disabled.HTTPRoutes()
	require.ErrorContains(t, err, "disabled")
	require.ErrorContains(t, disabled.configureHTTP(HTTPConfig{}), "frozen", "an unconfigured runtime stays headless once routes were requested")

	rt := httpRuntime(nil, false)
	require.ErrorContains(t, rt.configureHTTP(HTTPConfig{CustomerRoutes: []CustomerRoutesConfig{{Treasury: true}}}), "requires")
	require.Nil(t, rt.httpConfig, "failed validation cannot enable HTTP")
	policy := HTTPConfig{CustomerRoutes: []CustomerRoutesConfig{{Prefix: "/portal", DelegatedAuthenticator: rejectDelegated}}}
	require.NoError(t, rt.configureHTTP(policy))
	policy.Catalog = true
	policy.CustomerRoutes[0].Prefix = "/mutated"
	require.ErrorContains(t, rt.configureHTTP(HTTPConfig{}), "already configured")

	routes, err := rt.HTTPRoutes()
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
	again, err := rt.HTTPRoutes()
	require.NoError(t, err)
	require.Equal(t, original, again[0].Path)
	require.ErrorContains(t, rt.configureHTTP(HTTPConfig{}), "frozen")

	concurrent := httpRuntime(nil, false)
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for range 12 {
		wg.Go(func() { results <- concurrent.configureHTTP(HTTPConfig{}) })
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		}
	}
	require.Equal(t, 1, accepted)
}

func TestHTTPRouteExposureMatchesConfiguration(t *testing.T) {
	routes, err := httpRuntime(&HTTPConfig{}, false).HTTPRoutes()
	require.NoError(t, err)
	require.NotEmpty(t, routes)
	for _, r := range routes {
		for _, hidden := range []string{"/catalog", "/me/", "/merchant/"} {
			require.NotContains(t, r.Path, hidden, "an empty policy exposes no customer or management surface")
		}
	}

	full := &HTTPConfig{Checkout: true, MerchantAdmin: true, Catalog: true, MerchantConfig: true, MerchantAPI: true,
		CustomerRoutes: []CustomerRoutesConfig{{Treasury: true, DelegatedAuthenticator: rejectDelegated}}}
	rt := httpRuntime(full, true)
	rt.app.Config.SecretBackend = config.SecretBackendSnapshot
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
	rt := httpRuntime(&HTTPConfig{Catalog: true, MerchantAdmin: true}, true)
	rt.app.Config.AllowCatalogUpdates = false
	routes, err := rt.HTTPRoutes()
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
	verifier := billingauth.DelegatedAuthenticatorFunc(func(_ context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
		calls++
		require.Equal(t, target, r.RequestURI)
		require.Equal(t, "/api/pay/v1/customers/a%2Fb/invoices/invoice-1", r.URL.RawPath)
		require.Equal(t, "a/b", r.PathValue("customer_id"))
		require.Equal(t, "invoice-1", r.PathValue("id"))
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, body, string(raw))
		return nil, billingauth.ErrUnauthenticated
	})
	mux := mountAt(t, httpRuntime(&HTTPConfig{CustomerRoutes: []CustomerRoutesConfig{{Treasury: true, DelegatedAuthenticator: verifier}}}, false), "/api/pay")
	require.Equal(t, http.StatusUnauthorized, serve(mux, http.MethodGet, target, body, "Authorization", "Bearer signed").Code)
	require.Equal(t, 1, calls)

	// Routers that accept a trailing slash without redirecting still bind values.
	bound := bindHTTPPathValues("/v1/customers/{customer_id}/invoices/{id}", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
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
	rt := httpRuntime(&HTTPConfig{CustomerRoutes: []CustomerRoutesConfig{{Treasury: true, DelegatedAuthenticator: rejectDelegated}}}, false)
	rt.app.Config.RateLimits = &config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}, "default": {RequestsPerMinute: 60}}
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
	verifier := func(audience string) billingauth.DelegatedAuthenticator {
		return billingauth.DelegatedAuthenticatorFunc(func(_ context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
			calls[audience]++
			require.Contains(t, r.RequestURI, "?proof=original")
			if audience == "platform" {
				require.Equal(t, "host-selected", r.PathValue("slug"))
			}
			if r.Header.Get("Authorization") != "Bearer "+audience {
				return nil, billingauth.ErrUnauthenticated
			}
			return &billingauth.DelegatedPrincipal{MerchantID: "11111111-1111-4111-8111-111111111111", SubjectID: "22222222-2222-4222-8222-222222222222"}, nil
		})
	}
	rt := httpRuntime(&HTTPConfig{CustomerRoutes: []CustomerRoutesConfig{
		{Prefix: "/billing/v1/me", DelegatedAuthenticator: verifier("portal")},
		{Prefix: "/api/v1/merchants/{slug}/billing/me", Scope: CustomerSubscriptionManagement, DelegatedAuthenticator: verifier("platform")},
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
	routes, err := rt.HTTPRoutes()
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
	for _, prefix := range []string{"/", "/customer/", "/customer/../other", "/customer/{tail...}", "/customer/{slug}/%2f"} {
		rt := httpRuntime(nil, false)
		require.Error(t, rt.configureHTTP(HTTPConfig{CustomerRoutes: []CustomerRoutesConfig{{Prefix: prefix, DelegatedAuthenticator: rejectDelegated}}}), prefix)
	}
	rt := httpRuntime(nil, false)
	rt.delegatedAuthenticator = rejectDelegated
	require.ErrorContains(t, rt.configureHTTP(HTTPConfig{CustomerRoutes: []CustomerRoutesConfig{{Prefix: "/portal"}}}), "own authenticator",
		"the runtime's Client verifier is never an implicit HTTP authority")
	require.NoError(t, rt.configureHTTP(HTTPConfig{CustomerRoutes: []CustomerRoutesConfig{
		{Prefix: "/v1/me", DelegatedAuthenticator: rejectDelegated}, {Prefix: "/v1/me", DelegatedAuthenticator: rejectDelegated},
	}}))
	_, err := rt.HTTPRoutes()
	require.ErrorContains(t, err, "conflicting")
}

func TestCustomerBillingManagementScope(t *testing.T) {
	rt := httpRuntime(&HTTPConfig{CustomerRoutes: []CustomerRoutesConfig{{Prefix: "/v1/me", Scope: CustomerBillingManagement, DelegatedAuthenticator: rejectDelegated}}}, false)
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

// Customer mounts ignore ambient cookies unless the host installs cookie
// admission, and admission still requires the portal origin.
func TestCustomerCookieAdmission(t *testing.T) {
	calls := 0
	authn := billingauth.DelegatedAuthenticatorFunc(func(_ context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
		calls++
		if _, err := r.Cookie("session"); err != nil {
			return nil, billingauth.ErrUnauthenticated
		}
		return &billingauth.DelegatedPrincipal{MerchantID: "11111111-1111-4111-8111-111111111111", SubjectID: "22222222-2222-4222-8222-222222222222"}, nil
	})
	mux := mountAt(t, httpRuntime(&HTTPConfig{CustomerRoutes: []CustomerRoutesConfig{{Prefix: "/portal", Scope: CustomerSubscriptionManagement, DelegatedAuthenticator: authn}}}, false), "/api/pay")
	admission, err := billingauth.CookieAuthentication("https://portal.example")
	require.NoError(t, err)
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
				target = admission(target)
			}
			rec := httptest.NewRecorder()
			target.ServeHTTP(rec, req)
			require.Equal(t, tc.code, rec.Code, rec.Body.String())
			require.Equal(t, tc.authCalls, calls)
		})
	}
}
