package openrailsfiber

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestFiberReviewFullInventoryMountsNatively(t *testing.T) {
	cfg := &config.Config{CatalogSource: config.CatalogSourceAPI, MerchantConfigSource: config.MerchantConfigSourceAPI}
	delegated := billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
		return nil, billingauth.ErrUnauthenticated
	})
	graph := &app.App{Config: cfg, Runtime: &app.Runtime{Config: cfg, Auth: &billingauth.Integration{Authentication: billingauth.AuthenticationFunc(func(context.Context, *http.Request) (billingauth.Identity, error) {
		return billingauth.Identity{}, billingauth.ErrUnauthenticated
	}), Authorization: billingauth.AuthorizationFunc(func(context.Context, *http.Request, billingauth.Identity, billingauth.Requirement) error {
		return billingauth.ErrUnauthenticated
	})}}}
	policy := &embed.HTTPConfig{Checkout: true, CustomerRoutes: []embed.CustomerRoutesConfig{{Treasury: true, DelegatedAuthenticator: delegated}}, MerchantAdmin: true, Catalog: true, PaymentProviders: true, MerchantAPI: true}
	table, err := embedhttp.ConfiguredRoutes(graph, policy)
	require.NoError(t, err)
	b := &Bundle{}
	for _, route := range table.Entries {
		b.routes = append(b.routes, embed.HTTPRoute{Method: route.Method, Path: strings.TrimPrefix(route.Path, "/billing"), Handler: route.Handler})
	}
	engine := fiber.New(fiber.Config{CaseSensitive: true, StrictRouting: true})
	require.NoError(t, b.Mount(engine.Group("/api/pay")))
	for _, route := range engine.GetRoutes() {
		require.True(t, strings.HasPrefix(route.Name, RouteNamePrefix), route.Method+" "+route.Path)
	}
	gets := 0
	for _, r := range b.routes {
		if r.Method == http.MethodGet {
			gets++
		}
	}
	require.Equal(t, len(b.routes)+gets, len(engine.GetRoutes(true)))
	for _, route := range engine.GetRoutes(true) {
		require.NotContains(t, route.Path, "*")
	}
	for _, path := range []string{"/api/pay/v1/merchant/customers/entitlements:batch", "/api/pay/v1/merchant/customers/entitlementsXYZ"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer invalid")
		response, err := engine.Test(req)
		require.NoError(t, err)
		raw, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		response.Body.Close()
		if strings.HasSuffix(path, ":batch") {
			require.Equal(t, http.StatusUnauthorized, response.StatusCode, string(raw))
		} else {
			require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, response.StatusCode, string(raw))
		}
	}
}

type reviewContextKey struct{}

func TestFiberReviewRequestBodyContextAndHostFallback(t *testing.T) {
	engine := fiber.New()
	engine.Use(func(c fiber.Ctx) error {
		c.SetContext(context.WithValue(c.Context(), reviewContextKey{}, "host"))
		return c.Next()
	})
	const body = "{ \"whitespace\": true, \"unicode\": \"é\" }\n"
	const target = "/api/pay/v1/webhooks/stripe/acct_test?signature=original"
	calls := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, body, string(raw))
		require.Equal(t, target, r.RequestURI)
		require.Equal(t, "/api/pay/v1/webhooks/stripe/acct_test", r.URL.Path)
		require.Equal(t, "exact-signature", r.Header.Get("Stripe-Signature"))
		require.Equal(t, "host", r.Context().Value(reviewContextKey{}))
		w.WriteHeader(http.StatusNoContent)
	})
	b := &Bundle{routes: []embed.HTTPRoute{{Method: http.MethodPost, Path: "/v1/webhooks/{provider}/{account_id}", Handler: h}}}
	require.NoError(t, b.Mount(engine.Group("/api/pay")))
	engine.Use(func(c fiber.Ctx) error { return c.SendStatus(418) })
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Stripe-Signature", "exact-signature")
	response, err := engine.Test(req)
	require.NoError(t, err)
	response.Body.Close()
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	require.Equal(t, 1, calls)
	for _, path := range []string{"/other", "/api/pay/v1/unrelated", "/api/payment/v1/webhooks/stripe/acct_test"} {
		response, err = engine.Test(httptest.NewRequest(http.MethodPost, path, nil))
		require.NoError(t, err)
		response.Body.Close()
		require.Equal(t, 418, response.StatusCode)
	}
	require.Equal(t, 1, calls)
}
