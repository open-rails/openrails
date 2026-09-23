package openrailsgin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestGinReviewFullInventoryMountsNatively(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{AllowCatalogUpdates: true}
	delegated := billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
		return nil, billingauth.ErrUnauthenticated
	})
	graph := &app.App{Config: cfg, Runtime: &app.Runtime{Config: cfg, Auth: &billingauth.Integration{Authentication: billingauth.AuthenticationFunc(func(context.Context, *http.Request) (billingauth.Identity, error) {
		return billingauth.Identity{}, billingauth.ErrUnauthenticated
	}), Authorization: billingauth.AuthorizationFunc(func(context.Context, *http.Request, billingauth.Identity, billingauth.Requirement) error {
		return billingauth.ErrUnauthenticated
	})}}}
	policy := &embed.HTTPConfig{Checkout: true, CustomerRoutes: []embed.CustomerRoutesConfig{{Treasury: true, DelegatedAuthenticator: delegated}}, MerchantAdmin: true, Catalog: true, MerchantConfig: true, MerchantAPI: true}
	table, err := embedhttp.ConfiguredRoutes(graph, policy)
	require.NoError(t, err)
	b := &Bundle{}
	for _, route := range table.Entries {
		b.routes = append(b.routes, embed.HTTPRoute{Method: route.Method, Path: strings.TrimPrefix(route.Path, "/billing"), Handler: route.Handler})
	}
	engine := gin.New()
	engine.HandleMethodNotAllowed = true
	require.NoError(t, b.Mount(engine.Group("/api/pay")))
	gets := 0
	for _, r := range b.routes {
		if r.Method == http.MethodGet {
			gets++
		}
	}
	require.Equal(t, len(b.routes)+gets, len(engine.Routes()))
	for _, route := range engine.Routes() {
		require.NotContains(t, route.Path, "*")
	}
	for _, path := range []string{"/api/pay/v1/merchant/customers/entitlements:batch", "/api/pay/v1/merchant/customers/entitlementsXYZ"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer invalid")
		engine.ServeHTTP(w, req)
		if strings.HasSuffix(path, ":batch") {
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		} else {
			require.Equal(t, http.StatusMethodNotAllowed, w.Code, w.Body.String())
		}
	}
}

func TestGinReviewRequestBodyAndHostFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
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
		w.WriteHeader(http.StatusNoContent)
	})
	b := &Bundle{routes: []embed.HTTPRoute{{Method: http.MethodPost, Path: "/v1/webhooks/{provider}/{account_id}", Handler: h}}}
	require.NoError(t, b.Mount(engine.Group("/api/pay")))
	engine.NoRoute(func(c *gin.Context) { c.String(418, "host fallback") })
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Stripe-Signature", "exact-signature")
	engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, 1, calls)
	for _, path := range []string{"/other", "/api/pay/v1/unrelated", "/api/payment/v1/webhooks/stripe/acct_test"} {
		w = httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		require.Equal(t, 418, w.Code)
	}
	require.Equal(t, 1, calls)
}
