package openrailsfiber

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/routebundle"
	"github.com/open-rails/openrails/pkg/billingauth"
)

type routeSource struct {
	routes []embed.HTTPRoute
	root   bool
}

func (s routeSource) HTTPRoutes() ([]embed.HTTPRoute, error) { return s.routes, nil }
func (s routeSource) HTTPRequiresRoot() bool                 { return s.root }

func denyDelegated(calls *int) billingauth.DelegatedAuthenticator {
	return billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
		*calls++
		return nil, billingauth.ErrUnauthenticated
	})
}

// inventoryBundle builds every configured route family the way embed.Runtime does.
func inventoryBundle(t *testing.T) *Bundle {
	t.Helper()
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, AllowCatalogUpdates: true, SecretBackend: config.SecretBackendDB}
	auth := &billingauth.Integration{
		Authentication: billingauth.AuthenticationFunc(func(context.Context, *http.Request) (billingauth.Identity, error) {
			return billingauth.Identity{}, billingauth.ErrUnauthenticated
		}),
		Authorization: billingauth.AuthorizationFunc(func(context.Context, *http.Request, billingauth.Identity, billingauth.Requirement) error {
			return billingauth.ErrUnauthenticated
		}),
	}
	graph := &app.App{Config: cfg, Runtime: &app.Runtime{Config: cfg, Auth: auth}}
	policy := &embed.HTTPConfig{Checkout: true, CustomerRoutes: []embed.CustomerRoutesConfig{{Treasury: true, DelegatedAuthenticator: denyDelegated(new(int))}},
		MerchantAdmin: true, Catalog: true, MerchantConfig: true, MerchantAPI: true}
	table, err := embedhttp.ConfiguredRoutes(graph, policy)
	require.NoError(t, err)
	for i := range table.Entries {
		table.Entries[i].Path = strings.TrimPrefix(table.Entries[i].Path, "/billing")
	}
	require.NoError(t, embedhttp.ValidateRouteTable(table))
	bundle, err := Routes(routeSource{routes: routebundle.FromTable(table)})
	require.NoError(t, err)
	return bundle
}

func status(t *testing.T, engine *fiber.App, method, path string, body io.Reader) int {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer invalid")
	response, err := engine.Test(req)
	require.NoError(t, err)
	_ = response.Body.Close()
	return response.StatusCode
}

func strictApp() *fiber.App { return fiber.New(fiber.Config{CaseSensitive: true, StrictRouting: true}) }

func TestInventoryMountsNatively(t *testing.T) {
	b := inventoryBundle(t)
	engine := strictApp()
	require.NoError(t, b.Mount(engine.Group("/api/pay")))
	gets := 0
	for _, r := range b.routes {
		if r.Method == http.MethodGet {
			gets++
		}
	}
	require.Len(t, engine.GetRoutes(true), len(b.routes)+gets, "every GET gains a native HEAD")
	for _, route := range engine.GetRoutes(true) {
		require.True(t, strings.HasPrefix(route.Name, RouteNamePrefix), route.Method+" "+route.Path)
		require.NotContains(t, route.Path, "*", "no catch-all may widen the declared inventory")
	}
	for _, tc := range []struct {
		method, path string
		codes        []int
	}{
		{http.MethodGet, "/api/pay/v1/capabilities", []int{http.StatusOK}},
		{http.MethodHead, "/api/pay/v1/capabilities", []int{http.StatusOK}},
		{http.MethodPost, "/api/pay/v1/merchant/customers/entitlements:batch", []int{http.StatusUnauthorized}},
		{http.MethodPost, "/api/pay/v1/merchant/customers/entitlementsXYZ", []int{http.StatusNotFound, http.StatusMethodNotAllowed}},
		{http.MethodOptions, "/api/pay/v1/checkout", []int{http.StatusNoContent}},
		{http.MethodGet, "/API/pay/v1/capabilities", []int{http.StatusNotFound}},
	} {
		require.Contains(t, tc.codes, status(t, engine, tc.method, tc.path, nil), tc.method+" "+tc.path)
	}
}

type hostContextKey struct{}

// Signed webhooks need the exact bytes, URI and headers; host context values flow through.
func TestWebhookRequestReachesHandlerUnchanged(t *testing.T) {
	engine := fiber.New()
	engine.Use(func(c fiber.Ctx) error {
		c.SetContext(context.WithValue(c.Context(), hostContextKey{}, "host"))
		return c.Next()
	})
	const body = "{ \"whitespace\": true, \"unicode\": \"é\" }\n"
	const target = "/api/pay/v1/webhooks/stripe/acct_test?signature=original"
	calls := 0
	b := &Bundle{routes: []embed.HTTPRoute{{Method: http.MethodPost, Path: "/v1/webhooks/{provider}/{account_id}", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, body, string(raw))
		require.Equal(t, target, r.RequestURI)
		require.Equal(t, "/api/pay/v1/webhooks/stripe/acct_test", r.URL.Path)
		require.Equal(t, "exact-signature", r.Header.Get("Stripe-Signature"))
		require.Equal(t, "host", r.Context().Value(hostContextKey{}))
		w.WriteHeader(http.StatusNoContent)
	})}}}
	require.NoError(t, b.Mount(engine.Group("/api/pay")))
	engine.Use(func(c fiber.Ctx) error { return c.SendStatus(http.StatusTeapot) })
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Stripe-Signature", "exact-signature")
	response, err := engine.Test(req)
	require.NoError(t, err)
	_ = response.Body.Close()
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	for _, path := range []string{"/other", "/api/pay/v1/unrelated", "/api/payment/v1/webhooks/stripe/acct_test"} {
		require.Equal(t, http.StatusTeapot, status(t, engine, http.MethodPost, path, nil), path)
	}
	require.Equal(t, 1, calls)
}

func TestRootOnlyBundleRefusesGroupBeforeRegistration(t *testing.T) {
	b, err := Routes(routeSource{root: true, routes: []embed.HTTPRoute{{Method: http.MethodGet, Path: "/admin/{asset...}", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/admin/js/site.js?q=raw", r.RequestURI)
		w.WriteHeader(http.StatusNoContent)
	})}}})
	require.NoError(t, err)
	engine := fiber.New()
	require.ErrorContains(t, b.Mount(engine.Group("/outer")), "root")
	require.Empty(t, engine.GetRoutes())
	require.NoError(t, b.Mount(engine))
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		require.Equal(t, http.StatusNoContent, status(t, engine, method, "/admin/js/site.js?q=raw", nil))
	}
	var nilBundle *Bundle
	require.Error(t, nilBundle.Mount(engine))
	require.Error(t, (&Bundle{}).Mount(nil))
}

// Configured customer prefixes may use a merchant parameter; a literal prefix
// must never become a Fiber wildcard or greedy parameter.
func TestCustomerPrefixCannotWidenToANativeWildcard(t *testing.T) {
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, SecretBackend: config.SecretBackendDB, MerchantConfigHTTP: true, AllowCatalogUpdates: true}
	graph := &app.App{Config: cfg, Runtime: &app.Runtime{Config: cfg}}
	for _, prefix := range []string{"/portal/*audience", "/portal/+audience", "/api/v1/merchants/{slug}/billing/me"} {
		calls := 0
		policy := &embed.HTTPConfig{CustomerRoutes: []embed.CustomerRoutesConfig{{Prefix: prefix, Scope: embed.CustomerSubscriptionManagement, DelegatedAuthenticator: denyDelegated(&calls)}}}
		saas := strings.Contains(prefix, "{slug}")
		if err := embedhttp.ValidateHTTPConfig(policy, nil); err != nil {
			require.False(t, saas, err)
			continue
		}
		table, err := embedhttp.BuildCustomerRoutes(graph, policy.CustomerRoutes, nil)
		require.NoError(t, err)
		if err := embedhttp.ValidateRouteTable(table); err != nil {
			require.False(t, saas, err)
			continue
		}
		engine := strictApp()
		if err := (&Bundle{routes: routebundle.FromTable(table)}).Mount(engine); err != nil {
			require.False(t, saas, err)
			continue
		}
		require.Equal(t, http.StatusNotFound, status(t, engine, http.MethodPost, "/portal/unconfiguredaudience/subscriptions/not-id/cancel", nil),
			"%s broadened to an undeclared customer audience", prefix)
		if !saas {
			continue
		}
		for _, request := range []struct {
			method, path string
			status       int
		}{
			{http.MethodPost, "/api/v1/merchants/acme/billing/me/subscriptions/not-id/cancel", http.StatusUnauthorized},
			{http.MethodOptions, "/api/v1/merchants/acme/billing/me/subscriptions/not-id/cancel", http.StatusNoContent},
			{http.MethodPost, "/api/v1/merchants/acme/billing/me/checkout", http.StatusNotFound},
		} {
			require.Equal(t, request.status, status(t, engine, request.method, request.path, nil), request.method+" "+request.path)
		}
		require.Equal(t, 1, calls)
	}
}
