package openrailsgin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
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

func serve(engine http.Handler, method, path string, body io.Reader) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer invalid")
	engine.ServeHTTP(w, req)
	return w
}

func TestInventoryMountsNatively(t *testing.T) {
	gin.SetMode(gin.TestMode)
	b := inventoryBundle(t)
	engine := gin.New()
	engine.HandleMethodNotAllowed = true
	engine.NoRoute(func(c *gin.Context) { c.Status(http.StatusTeapot) })
	require.NoError(t, b.Mount(engine.Group("/api/pay")))
	gets := 0
	for _, r := range b.routes {
		if r.Method == http.MethodGet {
			gets++
		}
	}
	require.Len(t, engine.Routes(), len(b.routes)+gets, "every GET gains a native HEAD")
	for _, route := range engine.Routes() {
		require.NotContains(t, route.Path, "*", "no catch-all may widen the declared inventory")
	}
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/api/pay/v1/capabilities", http.StatusOK},
		{http.MethodHead, "/api/pay/v1/capabilities", http.StatusOK},
		{http.MethodPost, "/api/pay/v1/merchant/customers/entitlements:batch", http.StatusUnauthorized},
		{http.MethodPost, "/api/pay/v1/merchant/customers/entitlementsXYZ", http.StatusMethodNotAllowed},
		{http.MethodOptions, "/api/pay/v1/checkout", http.StatusNoContent},
		{http.MethodGet, "/api/payment/v1/capabilities", http.StatusTeapot},
	} {
		w := serve(engine, tc.method, tc.path, nil)
		require.Equal(t, tc.code, w.Code, "%s %s: %s", tc.method, tc.path, w.Body.String())
	}
}

// Signed webhooks need the exact bytes, URI and headers the provider sent.
func TestWebhookRequestReachesHandlerUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const body = "{ \"whitespace\": true, \"unicode\": \"é\" }\n"
	const target = "/api/pay/v1/webhooks/stripe/acct_test?signature=original"
	calls := 0
	b := &Bundle{routes: []embed.HTTPRoute{{Method: http.MethodPost, Path: "/v1/webhooks/{provider}/{account_id}", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, body, string(raw))
		require.Equal(t, target, r.RequestURI)
		require.Equal(t, "exact-signature", r.Header.Get("Stripe-Signature"))
		w.WriteHeader(http.StatusNoContent)
	})}}}
	engine := gin.New()
	require.NoError(t, b.Mount(engine.Group("/api/pay")))
	engine.NoRoute(func(c *gin.Context) { c.Status(http.StatusTeapot) })
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Stripe-Signature", "exact-signature")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusNoContent, w.Code)
	for _, path := range []string{"/other", "/api/pay/v1/unrelated", "/api/payment/v1/webhooks/stripe/acct_test"} {
		require.Equal(t, http.StatusTeapot, serve(engine, http.MethodPost, path, nil).Code, path)
	}
	require.Equal(t, 1, calls)
}

func TestRootOnlyBundleRefusesGroupBeforeRegistration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	b, err := Routes(routeSource{root: true, routes: []embed.HTTPRoute{{Method: http.MethodGet, Path: "/admin/{asset...}", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/admin/js/site.js?q=raw", r.RequestURI)
		w.WriteHeader(http.StatusNoContent)
	})}}})
	require.NoError(t, err)
	engine := gin.New()
	require.ErrorContains(t, b.Mount(engine.Group("/outer")), "root")
	require.Empty(t, engine.Routes())
	require.NoError(t, b.Mount(engine))
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		require.Equal(t, http.StatusNoContent, serve(engine, method, "/admin/js/site.js?q=raw", nil).Code)
	}
	var nilBundle *Bundle
	require.Error(t, nilBundle.Mount(engine))
	require.Error(t, (&Bundle{}).Mount(nil))
}

// Configured customer prefixes may use a merchant parameter, never a Gin wildcard.
func TestCustomerPrefixCannotWidenToANativeWildcard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, SecretBackend: config.SecretBackendDB, MerchantConfigHTTP: true, AllowCatalogUpdates: true}
	graph := &app.App{Config: cfg, Runtime: &app.Runtime{Config: cfg}}
	for _, tc := range []struct {
		prefixes []string
		valid    bool
	}{
		{[]string{"/portal/*audience"}, false},
		{[]string{"/audiences/{portal}/one", "/audiences/{platform}/two"}, false},
		{[]string{"/api/v1/merchants/{slug}/billing/me"}, true},
	} {
		calls := 0
		policy := &embed.HTTPConfig{}
		for _, prefix := range tc.prefixes {
			policy.CustomerRoutes = append(policy.CustomerRoutes, embed.CustomerRoutesConfig{Prefix: prefix, Scope: embed.CustomerSubscriptionManagement, DelegatedAuthenticator: denyDelegated(&calls)})
		}
		err := embedhttp.ValidateHTTPConfig(policy, nil)
		if err == nil {
			table, buildErr := embedhttp.BuildCustomerRoutes(graph, policy.CustomerRoutes, nil)
			require.NoError(t, buildErr)
			if err = embedhttp.ValidateRouteTable(table); err == nil {
				engine := gin.New()
				require.NoError(t, (&Bundle{routes: routebundle.FromTable(table)}).Mount(engine))
				for _, request := range []struct {
					method, path string
					status       int
				}{
					{http.MethodPost, "/api/v1/merchants/acme/billing/me/subscriptions/not-id/cancel", http.StatusUnauthorized},
					{http.MethodOptions, "/api/v1/merchants/acme/billing/me/subscriptions/not-id/cancel", http.StatusNoContent},
					{http.MethodPost, "/api/v1/merchants/acme/billing/me/checkout", http.StatusNotFound},
					{http.MethodPost, "/portal/anyone/subscriptions/not-id/cancel", http.StatusNotFound},
				} {
					w := serve(engine, request.method, request.path, nil)
					require.Equal(t, request.status, w.Code, "%s %s: %s", request.method, request.path, w.Body.String())
				}
				require.Equal(t, 1, calls)
			}
		}
		require.Equal(t, tc.valid, err == nil, "%v: %v", tc.prefixes, err)
	}
}
