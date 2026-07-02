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
	"github.com/open-rails/openrails/internal/http/embedhttp"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
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
	suite := getSharedTestSuite(t)
	asm := embedhttp.FromApp(suite.App)
	require.NotNil(t, asm)

	h := asm.NewHTTPHandler(embedhttp.Options{RouteSets: []embedhttp.RouteSet{embedhttp.RouteSetWebhooks}})

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
	suite := getSharedTestSuite(t)
	asm := embedhttp.FromApp(suite.App)
	require.NotNil(t, asm)
	// Default route sets include checkout/customer, which require an authenticator.
	asm.Authenticator = suiteTestAuthenticator{}

	embeddedDefault := httptest.NewServer(asm.NewHTTPHandler(embedhttp.Options{}))
	t.Cleanup(embeddedDefault.Close)
	require.Equal(t, http.StatusNotFound, status(t, embeddedDefault.Client(), http.MethodPost, embeddedDefault.URL+"/billing/v1/merchant/admissions"))
	require.NotEqual(t, http.StatusNotFound, status(t, embeddedDefault.Client(), http.MethodGet, embeddedDefault.URL+"/billing/v1/merchant/catalog/products"))
	require.Equal(t, http.StatusNotFound, status(t, embeddedDefault.Client(), http.MethodGet, embeddedDefault.URL+"/billing/v1/merchant/payment-providers/nmi"))

	embeddedMerchantAPI := httptest.NewServer(asm.NewHTTPHandler(embedhttp.Options{
		RouteSets: []embedhttp.RouteSet{embedhttp.RouteSetMerchantAPI},
	}))
	t.Cleanup(embeddedMerchantAPI.Close)
	require.NotEqual(t, http.StatusNotFound, status(t, embeddedMerchantAPI.Client(), http.MethodPost, embeddedMerchantAPI.URL+"/billing/v1/merchant/admissions"))

	embeddedPaymentProviders := httptest.NewServer(asm.NewHTTPHandler(embedhttp.Options{
		RouteSets: []embedhttp.RouteSet{embedhttp.RouteSetPaymentProviders},
	}))
	t.Cleanup(embeddedPaymentProviders.Close)
	require.NotEqual(t, http.StatusNotFound, status(t, embeddedPaymentProviders.Client(), http.MethodGet, embeddedPaymentProviders.URL+"/billing/v1/merchant/payment-providers/nmi"))

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
			asm.Gate = httproutes.NewGate(httproutes.GateOptions{DelegatedAuthenticator: testHostPrincipalAuthenticator{perms: tc.perms}})
			h := httptest.NewServer(asm.NewHTTPHandler(embedhttp.Options{
				RouteSets: []embedhttp.RouteSet{embedhttp.RouteSetCatalog},
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
			asm.Gate = httproutes.NewGate(httproutes.GateOptions{DelegatedAuthenticator: testHostPrincipalAuthenticator{perms: tc.perms}})
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
