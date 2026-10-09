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
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/routebundle"
)

// customerAuth is Deps.AuthenticateCustomer.
type customerAuth = func(r *http.Request, profile string) (*billingauth.DelegatedPrincipal, error)

// httpRuntime is a database-free runtime: route materialization and every
// refusal exercised here happen before a handler touches storage. Delegated
// customer profiles refuse every request unless customer says otherwise.
func httpRuntime(managementAuth bool, customer ...customerAuth) *Engine {
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
	c := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}
	authenticate := customerAuth(rejectDelegated)
	if len(customer) > 0 {
		authenticate = customer[0]
	}
	return &Engine{App: &app.App{Config: c, Runtime: &app.Runtime{Config: c, Auth: auth, AuthenticateCustomer: authenticate, CatalogEdits: &catalogpolicy.Exposure{}}}}
}

func delegated(scope config.CustomerHTTPScope, prefixes ...string) config.Routes {
	sel := config.Routes{}
	for _, prefix := range prefixes {
		sel.CustomerProfiles = append(sel.CustomerProfiles, config.CustomerRoutes{Prefix: prefix, Scope: scope, Delegated: true})
	}
	return sel
}

func rejectDelegated(*http.Request, string) (*billingauth.DelegatedPrincipal, error) {
	return nil, billingauth.ErrUnauthenticated
}

func mountAt(t *testing.T, rt *Engine, sel config.Routes, prefix string) *http.ServeMux {
	t.Helper()
	routes, err := rt.Routes(sel)
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

// Routes copies the selection: neither the host's later edits nor a caller's
// edits to returned routes leak into another mount.
func TestRoutesCopyTheSelection(t *testing.T) {
	rt := httpRuntime(false)
	sel := delegated(config.CustomerSelfService, "/portal")
	routes, err := rt.Routes(sel)
	require.NoError(t, err)
	sel.CustomerProfiles[0].Prefix = "/mutated"
	var portal bool
	for _, route := range routes {
		require.NotContains(t, route.Path, "/merchant/")
		require.NotContains(t, route.Path, "/mutated")
		portal = portal || strings.HasPrefix(route.Path, "/portal/")
	}
	require.True(t, portal)
	original := routes[0].Path
	routes[0].Path = "/caller-mutated"
	again, err := rt.Routes(delegated(config.CustomerSelfService, "/portal"))
	require.NoError(t, err)
	require.Equal(t, original, again[0].Path)

	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { _, err := rt.Routes(config.Routes{}); require.NoError(t, err) })
	}
	wg.Wait()
}

func TestHTTPRouteExposureMatchesSelection(t *testing.T) {
	routes, err := httpRuntime(false).Routes(config.Routes{})
	require.NoError(t, err)
	require.NotEmpty(t, routes, "webhooks and capabilities are always mounted")
	for _, r := range routes {
		for _, hidden := range []string{"/catalog", "/me/", "/merchant/", "/products", "/checkout-sessions"} {
			require.NotContains(t, r.Path, hidden, "an empty selection exposes no storefront, customer or merchant surface")
		}
	}

	full := delegated(config.CustomerSelfService, "/v1/me")
	full.Storefront, full.Merchant = true, true
	rt := httpRuntime(true)
	rt.App.Config.SecretBackend = config.SecretBackendSnapshot
	mux := mountAt(t, rt, full, "/api/pay")
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/api/pay/v1/me/balance", http.StatusUnauthorized},
		{http.MethodPost, "/api/pay/v1/me/checkout-sessions", http.StatusUnauthorized},
		{http.MethodPost, "/api/pay/v1/merchant/psps", http.StatusUnauthorized},
		{http.MethodOptions, "/api/pay/v1/me/balance", http.StatusNoContent},
		{http.MethodOptions, "/api/pay/v1/checkout-sessions/ocs_x/pay", http.StatusNoContent},
	} {
		rec := serve(mux, tc.method, tc.path, "{}", "Authorization", "Bearer invalid")
		require.Equal(t, tc.code, rec.Code, tc.method+" "+tc.path+" "+rec.Body.String())
	}
}

// The capability document reports what this mount serves.
func TestCapabilitiesReportTheMount(t *testing.T) {
	rt := httpRuntime(true)
	read := func(sel config.Routes) map[string]bool {
		rec := serve(mountAt(t, rt, sel, ""), http.MethodGet, "/v1/capabilities", "")
		require.Equal(t, http.StatusOK, rec.Code)
		var capabilities struct {
			RouteGroups map[string]bool `json:"route_groups"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &capabilities))
		return capabilities.RouteGroups
	}
	require.Equal(t, map[string]bool{"checkout": true, "customer": false, "merchant": false, "webhooks": true}, read(config.Routes{Storefront: true}))
	require.Equal(t, map[string]bool{"checkout": false, "customer": false, "merchant": true, "webhooks": true}, read(config.Routes{Merchant: true}))
	require.Equal(t, map[string]bool{"checkout": false, "customer": true, "merchant": false, "webhooks": true}, read(delegated(config.CustomerBillingManagement, "/v1/me")))
}

func TestCatalogEditsFollowTheMerchantMount(t *testing.T) {
	writes := func(rt *Engine, sel config.Routes) (reads, writes int) {
		routes, err := rt.Routes(sel)
		require.NoError(t, err)
		for _, route := range routes {
			if !strings.Contains(route.Path, "/catalog") || route.Method == http.MethodOptions {
				continue
			}
			// Offer lookup is a read with a POST body.
			if route.Method == http.MethodGet || route.Method == http.MethodHead || strings.HasSuffix(route.Path, "/offers/lookup") {
				reads++
			} else {
				writes++
			}
		}
		return reads, writes
	}
	closed := httpRuntime(true)
	reads, n := writes(closed, config.Routes{Merchant: true})
	require.Positive(t, reads)
	require.Zero(t, n, "catalog writes stay unmounted without CatalogEdits")
	require.False(t, closed.App.Runtime.CatalogEdits.Enabled())
	mux := mountAt(t, closed, config.Routes{Merchant: true}, "/api/pay")
	for _, path := range []string{"/api/pay/v1/merchant/catalog/products", "/api/pay/v1/merchant/catalog/prices"} {
		require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, serve(mux, http.MethodPost, path, "").Code, path)
	}
	_, err := closed.Routes(config.Routes{Merchant: true, CatalogEdits: true})
	require.ErrorContains(t, err, "every mount must agree")
	_, err = closed.Routes(config.Routes{Storefront: true})
	require.NoError(t, err, "a mount without the merchant API decides nothing")

	open := httpRuntime(true)
	_, n = writes(open, config.Routes{Merchant: true, CatalogEdits: true})
	require.Positive(t, n)
	require.True(t, open.App.Runtime.CatalogEdits.Enabled(), "the service guard follows the mount")

	_, err = httpRuntime(true).Routes(config.Routes{CatalogEdits: true})
	require.ErrorContains(t, err, "set Routes.Merchant")
	_, err = httpRuntime(false).Routes(config.Routes{Merchant: true})
	require.ErrorContains(t, err, "Deps.AuthorityFor")
}

// Delegated verifiers may check signatures over the exact request, so the
// adapter must hand them the original URI, raw path and unread body.
func TestHTTPVerifierSeesOriginalSignedRequest(t *testing.T) {
	const target = "/api/pay/v1/tenants/a%2Fb/me/invoices/invoice-1?view=raw"
	const body = "{ \"signed\" : \"unaltered\" }\n"
	calls := 0
	verifier := func(r *http.Request, profile string) (*billingauth.DelegatedPrincipal, error) {
		calls++
		require.Equal(t, "/v1/tenants/{tenant}/me", profile)
		require.Equal(t, target, r.RequestURI)
		require.Equal(t, "/api/pay/v1/tenants/a%2Fb/me/invoices/invoice-1", r.URL.RawPath)
		require.Equal(t, "a/b", r.PathValue("tenant"))
		require.Equal(t, "invoice-1", r.PathValue("id"))
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, body, string(raw))
		return nil, billingauth.ErrUnauthenticated
	}
	mux := mountAt(t, httpRuntime(false, verifier), delegated(config.CustomerSelfService, "/v1/tenants/{tenant}/me"), "/api/pay")
	require.Equal(t, http.StatusUnauthorized, serve(mux, http.MethodGet, target, body, "Authorization", "Bearer signed").Code)
	require.Equal(t, 1, calls)

	// Routers that accept a trailing slash without redirecting still bind values.
	bound := routebundle.BindPathValues("/v1/tenants/{tenant}/me/invoices/{id}", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		require.Equal(t, "acme", r.PathValue("tenant"))
		require.Equal(t, "invoice-1", r.PathValue("id"))
		require.Equal(t, "/api/pay/v1/tenants/acme/me/invoices/invoice-1/", r.URL.Path)
		calls++
	}))
	serve(bound, http.MethodGet, "/api/pay/v1/tenants/acme/me/invoices/invoice-1/", "")
	require.Equal(t, 2, calls)
}

// One limiter per runtime: a second host mount must not reset the counters.
func TestHTTPRateLimitIsSharedAcrossMountsAndRoutes(t *testing.T) {
	rt := httpRuntime(false)
	rt.App.Config.RateLimits = &config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}, "default": {RequestsPerMinute: 60}}
	sel := delegated(config.CustomerSelfService, "/v1/me")
	first, second := mountAt(t, rt, sel, "/first"), mountAt(t, rt, sel, "/second")
	for i, tc := range []struct {
		mux  http.Handler
		path string
	}{
		{first, "/first/v1/me/checkout-sessions"},
		{second, "/second/v1/me/checkout-sessions"},
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
			return &billingauth.DelegatedPrincipal{MerchantID: billing.MerchantID(uuid.MustParse("11111111-1111-4111-8111-111111111111")), SubjectID: "22222222-2222-4222-8222-222222222222"}, nil
		}
	}
	audiences := map[string]string{"/billing/v1/me": "portal", "/api/v1/merchants/{slug}/billing/me": "platform"}
	rt := httpRuntime(false, func(r *http.Request, profile string) (*billingauth.DelegatedPrincipal, error) {
		return verifier(audiences[profile])(r)
	})
	sel := config.Routes{CustomerProfiles: []config.CustomerRoutes{
		{Prefix: "/billing/v1/me", Scope: config.CustomerSelfService, Delegated: true},
		{Prefix: "/api/v1/merchants/{slug}/billing/me", Scope: config.CustomerSubscriptionManagement, Delegated: true},
	}}
	mux := mountAt(t, rt, sel, "/api/pay")
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
	for _, path := range []string{portal + "/merchant/customers", "/billing/v1/merchant/psps", platform + "/checkout-sessions", platform + "/payment-methods"} {
		require.Equal(t, http.StatusNotFound, serve(mux, http.MethodPost, "/api/pay"+path, "").Code, path)
	}
	routes, err := rt.Routes(sel)
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
	validate := func(rt *Engine, sel config.Routes) error {
		_, err := rt.Routes(sel)
		return err
	}
	for _, prefix := range []string{"/", "/customer/", "/customer/../other", "/customer/{tail...}", "/customer/{slug}/%2f"} {
		require.Error(t, validate(httpRuntime(false), delegated(config.CustomerSelfService, prefix)), prefix)
	}
	require.ErrorContains(t, validate(httpRuntime(false), config.Routes{Customers: config.CustomerSelfService}), "need Deps.AuthKit or Deps.Authenticate",
		"native customer routes need the host's authentication")
	require.ErrorContains(t, validate(httpRuntime(true), config.Routes{Customers: config.CustomerSelfService}), "need a merchant",
		"native customer routes need Config.Merchant")
	require.ErrorContains(t, validate(httpRuntime(false), delegated(config.CustomersNone, "/portal")), "need a Scope")
	unauthenticated := httpRuntime(false)
	unauthenticated.App.Runtime.AuthenticateCustomer = nil
	require.ErrorContains(t, validate(unauthenticated, delegated(config.CustomerSelfService, "/portal")), "Deps.AuthenticateCustomer")
	require.ErrorContains(t, validate(httpRuntime(false), delegated(config.CustomerSelfService, "/v1/me", "/v1/me")), "conflicting")
	require.ErrorContains(t, validate(httpRuntime(false), config.Routes{CookieOrigin: "http://portal.example"}), "CookieOrigin", "plain HTTP only on loopback")
}

func TestCustomerBillingManagementScope(t *testing.T) {
	mux := mountAt(t, httpRuntime(false), delegated(config.CustomerBillingManagement, "/v1/me"), "/api/pay")
	for _, path := range []string{"/payments", "/invoices", "/subscriptions", "/payment-methods"} {
		require.Equal(t, http.StatusUnauthorized, serve(mux, http.MethodGet, "/api/pay/v1/me"+path, "").Code, path)
	}
	for _, path := range []string{"/checkout-sessions", "/subscriptions/x/change-tier", "/billing-portal"} {
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

// Customer mounts ignore ambient cookies unless Routes.CookieOrigin admits
// them, and admission still requires that origin.
func TestCustomerCookieAdmission(t *testing.T) {
	calls := 0
	authn := func(r *http.Request, _ string) (*billingauth.DelegatedPrincipal, error) {
		calls++
		if _, err := r.Cookie("session"); err != nil {
			return nil, billingauth.ErrUnauthenticated
		}
		return &billingauth.DelegatedPrincipal{MerchantID: billing.MerchantID(uuid.MustParse("11111111-1111-4111-8111-111111111111")), SubjectID: "22222222-2222-4222-8222-222222222222"}, nil
	}
	sel := delegated(config.CustomerSubscriptionManagement, "/portal")
	mux := mountAt(t, httpRuntime(false, authn), sel, "/api/pay")
	sel.CookieOrigin = "https://portal.example"
	admitting := mountAt(t, httpRuntime(false, authn), sel, "/api/pay")
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

// The admin console mounts with the merchant API it drives, at its own path,
// pointed at the API's prefix and the host's AuthKit; otherwise Mount fails.
func TestAdminConsoleMountsWithTheMerchantAPI(t *testing.T) {
	rt := httpRuntime(true)
	console := &config.AdminConsole{Path: "/billing-admin"}
	_, err := rt.Routes(config.Routes{Prefix: "/billing", AdminConsole: console})
	require.ErrorContains(t, err, "set Routes.Merchant")
	sel := config.Routes{Prefix: "/billing", Merchant: true, AdminConsole: console}
	_, err = rt.Routes(sel)
	require.ErrorContains(t, err, "needs a console build")
	rt.App.ConsoleAssets = fstest.MapFS{"index.html": {Data: []byte(`<!doctype html><base href="/admin/">host build`)}}
	_, err = rt.Routes(sel)
	require.ErrorContains(t, err, "AuthBaseURL")
	rt.authAPIBase = "/api/v1"
	for _, bad := range []config.Routes{
		{Prefix: "billing", Merchant: true},
		{Prefix: "/billing", Merchant: true, AdminConsole: &config.AdminConsole{Path: "/admin/"}},
		{Prefix: "/billing", Merchant: true, AdminConsole: &config.AdminConsole{Path: "/billing/v1/admin"}},
	} {
		_, err = rt.Routes(bad)
		require.Error(t, err, "%+v", bad)
	}

	mux := mountAt(t, rt, sel, "")
	rec := serve(mux, http.MethodGet, "/billing-admin/", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `<base href="/billing-admin/">host build`)
	rec = serve(mux, http.MethodGet, "/billing-admin/config.json", "")
	require.JSONEq(t, `{"auth_base_url":"/api/v1","api_base_url":"/billing/v1","nl_widgets_enabled":false,"ask_enabled":false,"catalog_copilot_enabled":false,"catalog_drafting_enabled":false,"new_merchant_url":""}`, rec.Body.String())

	// A host's merchant-creation page reaches the console only through config.json.
	hosted := httpRuntime(true)
	hosted.App.ConsoleAssets, hosted.authAPIBase = rt.App.ConsoleAssets, "/api/v1"
	_, err = hosted.Routes(config.Routes{Merchant: true, AdminConsole: &config.AdminConsole{NewMerchantURL: "javascript:alert(1)"}})
	require.ErrorContains(t, err, "invalid Routes.AdminConsole.NewMerchantURL")
	hostedMux := mountAt(t, hosted, config.Routes{Merchant: true, AdminConsole: &config.AdminConsole{NewMerchantURL: "/merchants/new"}}, "")
	require.Contains(t, serve(hostedMux, http.MethodGet, "/admin/config.json", "").Body.String(), `"new_merchant_url":"/merchants/new"`)
	require.Equal(t, http.StatusOK, serve(mux, http.MethodGet, "/billing/v1/capabilities", "").Code)

	off := mountAt(t, httpRuntime(true), config.Routes{Prefix: "/billing", Merchant: true}, "")
	require.Equal(t, http.StatusNotFound, serve(off, http.MethodGet, "/admin/", "").Code, "no console unless selected")
}
