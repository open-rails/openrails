package openrailsgin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestReviewCustomerPrefixNativeCompatibility(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name     string
		prefixes []string
		valid    bool
	}{
		{"native wildcard", []string{"/portal/*audience"}, false},
		{"conflicting parameter names", []string{"/audiences/{portal}/one", "/audiences/{platform}/two"}, false},
		{"SaaS merchant parameter", []string{"/api/v1/merchants/{slug}/billing/me"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			reject := billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
				calls++
				return nil, billingauth.ErrUnauthenticated
			})
			cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, SecretBackend: config.SecretBackendDB, MerchantConfigHTTP: true, AllowCatalogUpdates: true}
			graph := &app.App{Config: cfg, Runtime: &app.Runtime{Config: cfg}}
			policy := &embed.HTTPConfig{}
			for _, prefix := range tc.prefixes {
				policy.CustomerRoutes = append(policy.CustomerRoutes, embed.CustomerRoutesConfig{Prefix: prefix, Scope: embed.CustomerSubscriptionManagement, DelegatedAuthenticator: reject})
			}
			engine := gin.New()
			err := embedhttp.ValidateHTTPConfig(policy, nil)
			if err != nil {
				require.False(t, tc.valid, err)
				require.Empty(t, engine.Routes())
				return
			}
			table, err := embedhttp.BuildCustomerRoutes(graph, policy.CustomerRoutes, nil)
			require.NoError(t, err)
			err = embedhttp.ValidateRouteTable(table)
			if !tc.valid {
				require.Error(t, err)
				require.Empty(t, engine.Routes())
				return
			}
			require.NoError(t, err)
			bundle := &Bundle{}
			for _, route := range table.Entries {
				bundle.routes = append(bundle.routes, embed.HTTPRoute{Method: route.Method, Path: route.Path, Handler: route.Handler})
			}
			require.NoError(t, bundle.Mount(engine))
			for _, request := range []struct {
				method, path string
				status       int
			}{
				{http.MethodPost, "/api/v1/merchants/acme/billing/me/subscriptions/not-id/cancel", 401},
				{http.MethodOptions, "/api/v1/merchants/acme/billing/me/subscriptions/not-id/cancel", 204},
				{http.MethodPost, "/api/v1/merchants/acme/billing/me/checkout", 404},
			} {
				rec := httptest.NewRecorder()
				engine.ServeHTTP(rec, httptest.NewRequest(request.method, request.path, nil))
				require.Equal(t, request.status, rec.Code, rec.Body.String())
			}
			require.Equal(t, 1, calls)
		})
	}
}
