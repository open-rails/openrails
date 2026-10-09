package embedhttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
)

func TestCapabilities(t *testing.T) {
	h := CapabilitiesHandler(nil, []RouteSet{RouteSetCheckout, RouteSetCustomer, RouteSetWebhooks}, routesurface.ProviderRoutes{Solana: true}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/capabilities", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "public, max-age=300", rec.Header().Get("Cache-Control"))
	var caps Capabilities
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &caps))
	require.Len(t, caps.RouteGroups, len(AllRouteSets), "every known group is reported")
	for _, rs := range AllRouteSets {
		require.Equal(t, rs == RouteSetCheckout || rs == RouteSetCustomer || rs == RouteSetWebhooks, caps.RouteGroups[string(rs)], rs)
	}
	require.Equal(t, map[string]bool{
		"solana_one_time_payments": true, "stripe_billing_portal": false,
		"solana_subscription_management": false, "provider_credential_writes": false,
		"api_host": false, "catalog_copilot": false, "metrics_ask": false, "dashboard_generation": false,
	}, caps.Features)

	req := httptest.NewRequest(http.MethodGet, "/billing/v1/capabilities", nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotModified, rec.Code)

	// Customer features follow the mounted customer scope, not provider support.
	for _, tc := range []struct {
		scope          config.CustomerHTTPScope
		portal, solana bool
	}{
		{config.CustomerSelfService, true, true},
		{config.CustomerBillingManagement, false, true},
		{config.CustomerSubscriptionManagement, false, true},
	} {
		caps := configuredCapabilities(nil, routeSets(config.Routes{}), []config.CustomerRoutes{{Scope: tc.scope}}, routesurface.AllProviderRoutes())
		require.True(t, caps.RouteGroups[string(RouteSetCustomer)])
		require.Equal(t, tc.portal, caps.Features["stripe_billing_portal"], tc.scope)
		require.Equal(t, tc.solana, caps.Features["solana_subscription_management"], tc.scope)
		require.False(t, caps.Features["provider_credential_writes"], "credential writes need the merchant group")
	}

	require.Equal(t, AllRouteSets, ResolveRouteSets(nil))
	require.Equal(t, []RouteSet{RouteSetCheckout, RouteSetWebhooks}, ResolveRouteSets([]RouteSet{RouteSetCheckout, "", RouteSetCheckout, RouteSetWebhooks}))
}

// Route selection is refused unless each surface has the Auth it needs:
// nothing mounts open.
func TestRoutesValidation(t *testing.T) {
	auth := &authtest.Fake{}
	customer := func(c config.CustomerRoutes) config.Routes {
		return config.Routes{CustomerProfiles: []config.CustomerRoutes{c}}
	}
	for _, tc := range []struct {
		name string
		sel  config.Routes
		ok   bool
	}{
		{"nothing selected", config.Routes{}, true},
		{"storefront needs no Auth: a session id is its credential", config.Routes{Storefront: true}, true},
		{"storefront", config.Routes{Storefront: true, Auth: auth}, true},
		{"merchant without Auth", config.Routes{Merchant: true}, false},
		{"merchant with a typed nil Auth", config.Routes{Merchant: true, Auth: (*authtest.Fake)(nil)}, false},
		{"merchant", config.Routes{Merchant: true, Auth: auth}, true},
		{"merchant configuration without Auth", config.Routes{MerchantConfig: true}, false},
		{"merchant configuration alone", config.Routes{MerchantConfig: true, Auth: auth}, true},
		{"customer without Auth", customer(config.CustomerRoutes{Merchant: "store", Scope: config.CustomerSelfService}), false},
		{"customer with the mount's Auth", config.Routes{Auth: auth, CustomerProfiles: []config.CustomerRoutes{{Scope: config.CustomerSelfService}}}, true},
		{"customer with its own Auth", customer(config.CustomerRoutes{Scope: config.CustomerSelfService, Auth: auth}), true},
		{"customers shorthand without Auth", config.Routes{Customers: config.CustomerBillingManagement}, false},
		{"customers shorthand", config.Routes{Customers: config.CustomerBillingManagement, Auth: auth}, true},
		{"no scope", customer(config.CustomerRoutes{Auth: auth}), false},
		{"unknown scope", customer(config.CustomerRoutes{Scope: 9, Auth: auth}), false},
		{"parameterized prefix", customer(config.CustomerRoutes{Prefix: "/v1/tenants/{tenant}/me", Scope: config.CustomerSelfService, Auth: auth}), true},
	} {
		require.Equal(t, tc.ok, ValidateRoutes(tc.sel, CustomerProfiles(tc.sel), &app.Runtime{}) == nil, tc.name)
	}
	for _, prefix := range []string{"/", "me", "/a/../b", "/a/", "/a/*", "/a b", "/a/{x.y}", "/a/b{c}", "/a/{}"} {
		sel := customer(config.CustomerRoutes{Prefix: prefix, Scope: config.CustomerSelfService, Auth: auth})
		require.Error(t, ValidateRoutes(sel, CustomerProfiles(sel), &app.Runtime{}), prefix)
	}
}

// The combined handler refuses to mount the merchant API without its Auth,
// and only checkout routes join the permissive-CORS browser tier.
func TestNewRoutes(t *testing.T) {
	require.Panics(t, func() { (&Assembler{}).NewRoutes(Options{RouteSets: []RouteSet{RouteSetMerchant}}) })

	asm := &Assembler{Runtime: &app.Runtime{Config: &config.Config{}}, Auth: &authtest.Fake{}}
	noWebhooks := routesurface.ProviderRoutes{}
	guard, err := StaffGuard(asm.Runtime, config.Routes{Merchant: true, Guards: authtest.StaffGuards()})
	require.NoError(t, err)
	table := asm.NewRoutes(Options{RouteSets: []RouteSet{RouteSetCheckout, RouteSetMerchant, RouteSetWebhooks}, ProviderRoutes: &noWebhooks, Guard: guard})
	var keys []string
	for _, e := range table.Entries {
		key := e.Method + " " + e.Path
		keys = append(keys, key)
		require.Equal(t, strings.HasPrefix(e.Path, "/billing/v1/checkout") || strings.HasPrefix(e.Path, "/billing/v1/captcha") ||
			slices.Contains([]string{"/billing/v1/products", "/billing/v1/checkout-config", "/billing/v1/currencies"}, e.Path), e.Browser, key)
		require.False(t, strings.HasPrefix(e.Path, "/billing/v1/webhooks/"), "callbacks need a webhook-capable rail")
	}
	require.Contains(t, keys, "GET /billing/v1/capabilities")
	require.Contains(t, keys, "OPTIONS /billing/v1/checkout-sessions/{id}/pay")
	require.Contains(t, keys, "GET /billing/v1/merchant/payments")
	require.NotContains(t, keys, "OPTIONS /billing/v1/merchant/payments")
}

// Provider credential writes need a DB secret backend that can write; an
// explicit route override can neither grant them nor invent a Solana signer.
func TestProviderRoutesForRuntime(t *testing.T) {
	for _, source := range []string{config.SecretBackendSnapshot, config.SecretBackendDB} {
		for _, writable := range []bool{false, true} {
			for _, explicit := range []bool{false, true} {
				rt := &app.Runtime{Config: &config.Config{SecretBackend: source}, RouteCapabilities: &routesurface.RuntimeCapabilities{SecretWrite: writable}}
				var override *routesurface.ProviderRoutes
				if explicit {
					all := routesurface.AllProviderRoutes()
					override = &all
				}
				routes := ProviderRoutesForRuntime(rt, override)
				require.Equal(t, source == config.SecretBackendDB && writable, routes.SecretWrite, "%s/%v/%v", source, writable, explicit)
				require.False(t, routes.SolanaSigning)
				require.True(t, routes.Webhooks)
				require.Equal(t, writable, rt.RouteCapabilities.SecretWrite, "route gating never mutates runtime capabilities")
			}
		}
	}
}

// Native adapters mirror gin's tree: catch-all subtrees own their descendants
// and one wildcard name per position.
func TestValidateRouteTable(t *testing.T) {
	h := http.NotFoundHandler()
	entry := func(method, path string) router.Entry { return router.Entry{Method: method, Path: path, Handler: h} }
	console, customer := entry("GET", "/admin/{asset...}"), entry("GET", "/admin/custom/balance")
	for _, entries := range [][]router.Entry{
		{console, customer}, {customer, console},
		{entry("GET", "/a/{id}/x"), entry("GET", "/a/{name}/y")},
		{entry("GET", "/dup"), entry("GET", "/dup")},
	} {
		require.Error(t, ValidateRouteTable(&router.Table{Entries: entries}), "%v", entries)
	}
	require.NoError(t, ValidateRouteTable(&router.Table{Entries: []router.Entry{
		console, entry("GET", "/admin"), entry("POST", "/admin/custom/subscriptions/{id}/cancel"),
		entry("GET", "/a/{id}/x"), entry("POST", "/a/{name}/y"),
	}}))
}
