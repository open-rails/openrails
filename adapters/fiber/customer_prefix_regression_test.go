package openrailsfiber

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestReviewCustomerPrefixMustNotBecomeNativeWildcard(t *testing.T) {
	reject := billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
		return nil, billingauth.ErrUnauthenticated
	})
	cfg := &config.Config{MerchantConfigSource: config.MerchantConfigSourceAPI, CatalogSource: config.CatalogSourceAPI}
	graph := &app.App{Config: cfg, Runtime: &app.Runtime{Config: cfg}}
	for _, prefix := range []string{"/portal/*audience", "/portal/+audience"} {
		t.Run(prefix, func(t *testing.T) {
			policy := &embed.HTTPConfig{CustomerRoutes: []embed.CustomerRoutesConfig{{Prefix: prefix, Scope: embed.CustomerSubscriptionManagement, DelegatedAuthenticator: reject}}}
			if embedhttp.ValidateHTTPConfig(policy, nil) != nil {
				return
			}
			table, err := embedhttp.BuildCustomerRoutes(graph, policy.CustomerRoutes, nil)
			require.NoError(t, err)
			if embedhttp.ValidateRouteTable(table) != nil {
				return
			}
			bundle := &Bundle{}
			for _, route := range table.Entries {
				bundle.routes = append(bundle.routes, embed.HTTPRoute{Method: route.Method, Path: route.Path, Handler: route.Handler})
			}
			engine := fiber.New(fiber.Config{CaseSensitive: true, StrictRouting: true})
			if bundle.Mount(engine) != nil {
				return
			}
			response, err := engine.Test(httptest.NewRequest(http.MethodPost, "/portal/unconfiguredaudience/subscriptions/not-id/cancel", nil))
			require.NoError(t, err)
			defer response.Body.Close()
			require.Equal(t, http.StatusNotFound, response.StatusCode, "literal prefix broadened to an undeclared customer audience")
		})
	}
}

func TestReviewSaaSCustomerPrefixMountsNatively(t *testing.T) {
	calls := 0
	reject := billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
		calls++
		return nil, billingauth.ErrUnauthenticated
	})
	cfg := &config.Config{MerchantConfigSource: config.MerchantConfigSourceAPI, CatalogSource: config.CatalogSourceAPI}
	graph := &app.App{Config: cfg, Runtime: &app.Runtime{Config: cfg}}
	policy := &embed.HTTPConfig{CustomerRoutes: []embed.CustomerRoutesConfig{{Prefix: "/api/v1/merchants/{slug}/billing/me", Scope: embed.CustomerSubscriptionManagement, DelegatedAuthenticator: reject}}}
	require.NoError(t, embedhttp.ValidateHTTPConfig(policy, nil))
	table, err := embedhttp.BuildCustomerRoutes(graph, policy.CustomerRoutes, nil)
	require.NoError(t, err)
	require.NoError(t, embedhttp.ValidateRouteTable(table))
	bundle := &Bundle{}
	for _, route := range table.Entries {
		bundle.routes = append(bundle.routes, embed.HTTPRoute{Method: route.Method, Path: route.Path, Handler: route.Handler})
	}
	engine := fiber.New(fiber.Config{CaseSensitive: true, StrictRouting: true})
	require.NoError(t, bundle.Mount(engine))
	for _, request := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, "/api/v1/merchants/acme/billing/me/subscriptions/not-id/cancel", 401},
		{http.MethodOptions, "/api/v1/merchants/acme/billing/me/subscriptions/not-id/cancel", 204},
		{http.MethodPost, "/api/v1/merchants/acme/billing/me/checkout", 404},
	} {
		response, err := engine.Test(httptest.NewRequest(request.method, request.path, nil))
		require.NoError(t, err)
		response.Body.Close()
		require.Equal(t, request.status, response.StatusCode)
	}
	require.Equal(t, 1, calls)
}
