package openrailsfiber

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth/authtest"

	"github.com/gofiber/fiber/v3"
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

func (b *Bundle) Mount(target *fiber.App) error {
	if b == nil || target == nil {
		return errors.New("nil bundle or router")
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
	require.NoError(t, b.Mount(engine))
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
		{http.MethodPost, "/api/pay/v1/merchant/entitlements/lookup", []int{http.StatusUnauthorized}},
		{http.MethodPost, "/api/pay/v1/merchant/customers/entitlementsXYZ", []int{http.StatusNotFound, http.StatusMethodNotAllowed}},
		{http.MethodOptions, "/api/pay/v1/checkout-sessions/ocs_x/pay", []int{http.StatusNoContent}},
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
	b := &Bundle{routes: []openrails.Route{{Method: http.MethodPost, Path: "/api/pay/v1/webhooks/{provider}/{account_id}", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	require.NoError(t, b.Mount(engine))
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

// A subtree route (the admin console's assets) mounts at the root, for GET
// and HEAD.
func TestSubtreeRouteMountsAtTheRoot(t *testing.T) {
	b := &Bundle{routes: []openrails.Route{{Method: http.MethodGet, Path: "/admin/{asset...}", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/admin/js/site.js?q=raw", r.RequestURI)
		w.WriteHeader(http.StatusNoContent)
	})}}}
	engine := fiber.New()
	require.NoError(t, b.Mount(engine))
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		require.Equal(t, http.StatusNoContent, status(t, engine, method, "/admin/js/site.js?q=raw", nil))
	}
	var nilBundle *Bundle
	require.Error(t, nilBundle.Mount(engine))
	require.Error(t, (&Bundle{}).Mount(nil))
	require.ErrorContains(t, Mount(engine, nil, openrails.Routes{}), "client")
}

// Configured customer prefixes may use a merchant parameter; a literal prefix
// must never become a Fiber wildcard or greedy parameter.
func TestCustomerPrefixCannotWidenToANativeWildcard(t *testing.T) {
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, SecretBackend: config.SecretBackendDB}
	rt := &app.Runtime{Config: cfg}
	rt.SetConfiguredMerchant(testMerchant)
	graph := &app.App{Config: cfg, Runtime: rt}
	for _, prefix := range []string{"/portal/*audience", "/portal/+audience", "/api/v1/merchants/{slug}/billing/me"} {
		fake := &authtest.Fake{}
		profiles := []config.CustomerRoutes{{Prefix: prefix, Scope: config.CustomerSubscriptionManagement, Auth: fake}}
		hosted := strings.Contains(prefix, "{slug}")
		if err := embedhttp.ValidateRoutes(config.Routes{CustomerProfiles: profiles}, profiles, graph.Runtime); err != nil {
			require.False(t, hosted, err)
			continue
		}
		table, err := embedhttp.BuildCustomerRoutes(graph, profiles)
		require.NoError(t, err)
		if err := embedhttp.ValidateRouteTable(table); err != nil {
			require.False(t, hosted, err)
			continue
		}
		engine := strictApp()
		if err := (&Bundle{routes: toRoutes(routebundle.FromTable(table))}).Mount(engine); err != nil {
			require.False(t, hosted, err)
			continue
		}
		require.Equal(t, http.StatusNotFound, status(t, engine, http.MethodPost, "/portal/unconfiguredaudience/subscriptions/not-id/cancel", nil),
			"%s broadened to an undeclared customer audience", prefix)
		if !hosted {
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
		require.Equal(t, 1, fake.Refused("Required"))
	}
}

// A client that serves no payment page lets only itself frame one.
func TestCheckoutFramePolicy(t *testing.T) {
	engine := strictApp()
	engine.Get("/checkout", CheckoutFramePolicy(nil), func(c fiber.Ctx) error { return c.SendString("page") })
	response, err := engine.Test(httptest.NewRequest(http.MethodGet, "/checkout", nil))
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, "frame-ancestors 'self'", response.Header.Get("Content-Security-Policy"))
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, "page", string(body))
}

var testMerchant = billing.MerchantID(uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
