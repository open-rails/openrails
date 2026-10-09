package openrailsgin

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

	"github.com/gin-gonic/gin"
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

func (b *Bundle) Mount(target *gin.Engine) error {
	if b == nil || target == nil {
		return errors.New("nil bundle or router")
	}
	return MountRoutes(target, b.routes)
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
	require.NoError(t, b.Mount(engine))
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
		{http.MethodPost, "/api/pay/v1/merchant/product-access", http.StatusUnauthorized},
		{http.MethodPost, "/api/pay/v1/merchant/customers/entitlementsXYZ/billing-profile", http.StatusMethodNotAllowed},
		{http.MethodOptions, "/api/pay/v1/checkout-sessions/ocs_x/pay", http.StatusNoContent},
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
	b := &Bundle{routes: []openrails.Route{{Method: http.MethodPost, Path: "/api/pay/v1/webhooks/{provider}/{account_id}", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, body, string(raw))
		require.Equal(t, target, r.RequestURI)
		require.Equal(t, "exact-signature", r.Header.Get("Stripe-Signature"))
		w.WriteHeader(http.StatusNoContent)
	})}}}
	engine := gin.New()
	require.NoError(t, b.Mount(engine))
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

// A subtree route (the admin console's assets) mounts at the root, for GET
// and HEAD.
func TestSubtreeRouteMountsAtTheRoot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	b := &Bundle{routes: []openrails.Route{{Method: http.MethodGet, Path: "/admin/{asset...}", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/admin/js/site.js?q=raw", r.RequestURI)
		w.WriteHeader(http.StatusNoContent)
	})}}}
	engine := gin.New()
	require.NoError(t, b.Mount(engine))
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		require.Equal(t, http.StatusNoContent, serve(engine, method, "/admin/js/site.js?q=raw", nil).Code)
	}
	var nilBundle *Bundle
	require.Error(t, nilBundle.Mount(engine))
	require.Error(t, (&Bundle{}).Mount(nil))
	require.ErrorContains(t, Mount(engine, nil, openrails.Routes{}), "client")
}

// Configured customer prefixes may use a merchant parameter, never a Gin wildcard.
func TestCustomerPrefixCannotWidenToANativeWildcard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, SecretBackend: config.SecretBackendDB}
	rt := &app.Runtime{Config: cfg}
	rt.SetConfiguredMerchant(testMerchant)
	graph := &app.App{Config: cfg, Runtime: rt}
	for _, tc := range []struct {
		prefixes []string
		valid    bool
	}{
		{[]string{"/portal/*audience"}, false},
		{[]string{"/audiences/{portal}/one", "/audiences/{platform}/two"}, false},
		{[]string{"/api/v1/merchants/{slug}/billing/me"}, true},
	} {
		fake := &authtest.Fake{}
		var profiles []config.CustomerRoutes
		for _, prefix := range tc.prefixes {
			profiles = append(profiles, config.CustomerRoutes{Prefix: prefix, Scope: config.CustomerSubscriptionManagement, Auth: fake})
		}
		err := embedhttp.ValidateRoutes(config.Routes{CustomerProfiles: profiles}, profiles, graph.Runtime)
		if err == nil {
			table, buildErr := embedhttp.BuildCustomerRoutes(graph, profiles, nil)
			require.NoError(t, buildErr)
			if err = embedhttp.ValidateRouteTable(table); err == nil {
				engine := gin.New()
				require.NoError(t, (&Bundle{routes: toRoutes(routebundle.FromTable(table))}).Mount(engine))
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
				require.Equal(t, 1, fake.Refused("Required"))
			}
		}
		require.Equal(t, tc.valid, err == nil, "%v: %v", tc.prefixes, err)
	}
}

// A client that serves no payment page lets only itself frame one.
func TestCheckoutFramePolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/checkout", CheckoutFramePolicy(nil), func(c *gin.Context) { c.String(http.StatusOK, "page") })
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/checkout", nil))
	require.Equal(t, "frame-ancestors 'self'", w.Header().Get("Content-Security-Policy"))
	require.Equal(t, "page", w.Body.String())
}

var testMerchant = billing.MerchantID(uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
