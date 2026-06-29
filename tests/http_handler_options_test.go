//go:build integration

package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	server "github.com/open-rails/openrails/internal/http"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/pkg/billingauth"
)

type testHostPrincipalAuthenticator struct {
	perms []string
}

func (a testHostPrincipalAuthenticator) AuthenticateDelegated(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
	return &billingauth.DelegatedPrincipal{
		MerchantID:   dbtest.TestMerchantID.String(),
		MerchantSlug: dbtest.TestMerchantSlug,
		SubjectID:    "11111111-1111-4111-8111-111111111111",
		Email:        "host-principal@test.example",
		Permissions:  append([]string(nil), a.perms...),
	}, nil
}

func TestHTTPHandlerOptions_WebhooksOnly(t *testing.T) {
	srv := setupTestServer(t)
	require.NotNil(t, srv)

	h := srv.NewHTTPHandler(server.HTTPHandlerOptions{RouteSets: []server.RouteSet{server.RouteSetWebhooks}})

	// Merchant-scoped webhook route should exist; global /webhooks is not mounted.
	{
		req := httptest.NewRequest(http.MethodPost, "/billing/v1/merchants/"+dbtest.TestMerchantSlug+"/webhooks/stripe", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.NotEqual(t, http.StatusNotFound, w.Code)
	}
	{
		req := httptest.NewRequest(http.MethodPost, "/billing/v1/webhooks/stripe", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code)
	}

	// User routes should be excluded.
	{
		req := httptest.NewRequest(http.MethodGet, "/billing/v1/products", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code)
	}

	// Embedded handler should not accept stripped-prefix paths.
	{
		req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code)
	}

	// Admin routes should be excluded.
	{
		req := httptest.NewRequest(http.MethodGet, "/billing/v1/merchant/metrics/summary", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code)
	}

	// Embedded handler must not expose standalone health endpoints.
	{
		req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code)
	}
}

func TestHTTPHandlerOptions_RouteSetPresetsOverHTTPServer(t *testing.T) {
	srv := setupTestServer(t)
	require.NotNil(t, srv)

	embeddedDefault := httptest.NewServer(srv.NewHTTPHandler(server.HTTPHandlerOptions{}))
	t.Cleanup(embeddedDefault.Close)
	require.Equal(t, http.StatusNotFound, status(t, embeddedDefault.Client(), http.MethodPost, embeddedDefault.URL+"/billing/v1/merchant/admissions"))
	require.Equal(t, http.StatusNotFound, status(t, embeddedDefault.Client(), http.MethodGet, embeddedDefault.URL+"/billing/v1/merchant/catalog/products"))

	embeddedMerchantAPI := httptest.NewServer(srv.NewHTTPHandler(server.HTTPHandlerOptions{
		RouteSets: []server.RouteSet{server.RouteSetMerchantAPI},
	}))
	t.Cleanup(embeddedMerchantAPI.Close)
	require.NotEqual(t, http.StatusNotFound, status(t, embeddedMerchantAPI.Client(), http.MethodPost, embeddedMerchantAPI.URL+"/billing/v1/merchant/admissions"))

	embeddedMerchantSettings := httptest.NewServer(srv.NewHTTPHandler(server.HTTPHandlerOptions{
		RouteSets: []server.RouteSet{server.RouteSetMerchantSettings},
	}))
	t.Cleanup(embeddedMerchantSettings.Close)
	require.NotEqual(t, http.StatusNotFound, status(t, embeddedMerchantSettings.Client(), http.MethodGet, embeddedMerchantSettings.URL+"/billing/v1/merchant/catalog/products"))

	standalone := httptest.NewServer(srv.Handler())
	t.Cleanup(standalone.Close)
	require.NotEqual(t, http.StatusNotFound, status(t, standalone.Client(), http.MethodPost, standalone.URL+"/v1/merchant/admissions"))
	require.NotEqual(t, http.StatusNotFound, status(t, standalone.Client(), http.MethodGet, standalone.URL+"/v1/merchant/catalog/products"))
}

func TestHTTPHandlerOptions_MerchantRoutesAcceptHostPrincipalPermissions(t *testing.T) {
	suite := getSharedTestSuite(t)

	settingsCases := []struct {
		name  string
		perms []string
		want  int
	}{
		{name: "exact settings perm", perms: []string{controlplane.PermMerchantCatalogRead}, want: http.StatusOK},
		{name: "merchant glob settings perm", perms: []string{"merchant:*"}, want: http.StatusOK},
		{name: "wrong namespace settings perm", perms: []string{controlplane.PermCustomerSpendDelegationsRead}, want: http.StatusForbidden},
	}
	for _, tc := range settingsCases {
		t.Run(tc.name, func(t *testing.T) {
			asm := embedhttp.FromApp(suite.App)
			asm.DelegatedAuthenticator = testHostPrincipalAuthenticator{perms: tc.perms}
			// Merchant settings routes require mutable-credentials mode.
			h := httptest.NewServer(asm.NewHTTPHandler(embedhttp.Options{
				RouteSets:      []embedhttp.RouteSet{embedhttp.RouteSetMerchantSettings},
				CredentialMode: embedhttp.CredentialModeMutable,
			}))
			t.Cleanup(h.Close)

			require.Equal(t, tc.want, status(t, h.Client(), http.MethodGet, h.URL+"/billing/v1/merchant/catalog/products"))
		})
	}

	apiCases := []struct {
		name  string
		perms []string
		want  int
	}{
		{name: "exact api perm", perms: []string{controlplane.PermMerchantAdmissionsCreate}, want: http.StatusBadRequest},
		{name: "merchant glob api perm", perms: []string{"merchant:*"}, want: http.StatusBadRequest},
		{name: "wrong namespace api perm", perms: []string{controlplane.PermCustomerSpendDelegationsRead}, want: http.StatusForbidden},
	}
	for _, tc := range apiCases {
		t.Run(tc.name, func(t *testing.T) {
			asm := embedhttp.FromApp(suite.App)
			asm.ServiceCredentialResolver = nil
			asm.DelegatedAuthenticator = testHostPrincipalAuthenticator{perms: tc.perms}
			h := httptest.NewServer(asm.NewHTTPHandler(embedhttp.Options{
				RouteSets: []embedhttp.RouteSet{embedhttp.RouteSetMerchantAPI},
			}))
			t.Cleanup(h.Close)

			require.Equal(t, tc.want, status(t, h.Client(), http.MethodPost, h.URL+"/billing/v1/merchant/admissions"))
		})
	}
}

func status(t *testing.T, client *http.Client, method, url string) int {
	t.Helper()

	req, err := http.NewRequest(method, url, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}
