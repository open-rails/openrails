package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestCustomerExposureProfilesKeepAuthorityAndOriginalRequest(t *testing.T) {
	calls := map[string]int{}
	verifier := func(audience string) billingauth.DelegatedAuthenticator {
		return billingauth.DelegatedAuthenticatorFunc(func(_ context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
			calls[audience]++
			require.Contains(t, r.RequestURI, "?proof=original")
			if audience == "platform" {
				require.Equal(t, "host-selected", r.PathValue("slug"))
				require.Equal(t, "/api/pay/api/v1/merchants/host-selected/billing/me/subscriptions/not-id/cancel", r.URL.Path)
			}
			if r.Header.Get("Authorization") != "Bearer "+audience {
				return nil, billingauth.ErrUnauthenticated
			}
			return &billingauth.DelegatedPrincipal{MerchantID: "11111111-1111-4111-8111-111111111111", SubjectID: "22222222-2222-4222-8222-222222222222"}, nil
		})
	}
	runtime := reviewRuntime(nil, nil)
	cfg := HTTPConfig{CustomerExposures: []CustomerHTTPConfig{
		{Prefix: "/billing/v1/me", DelegatedAuthenticator: verifier("portal")},
		{Prefix: "/api/v1/merchants/{slug}/billing/me", Scope: CustomerSubscriptionManagement, DelegatedAuthenticator: verifier("platform")},
	}}
	require.NoError(t, runtime.ConfigureHTTP(cfg))
	cfg.CustomerExposures[0].Prefix = "/mutated"
	mux := reviewMount(t, runtime)
	for _, tc := range []struct {
		path, token string
		status      int
	}{
		{"/billing/v1/me/subscriptions/not-id/cancel", "portal", 400},
		{"/billing/v1/me/subscriptions/not-id/cancel", "platform", 401},
		{"/api/v1/merchants/host-selected/billing/me/subscriptions/not-id/cancel", "portal", 401},
		{"/api/v1/merchants/host-selected/billing/me/subscriptions/not-id/cancel", "platform", 400},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/pay"+tc.path+"?proof=original", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+tc.token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		require.Equal(t, tc.status, rec.Code, rec.Body.String())
	}
	require.Equal(t, map[string]int{"portal": 2, "platform": 2}, calls)
	for _, path := range []string{
		"/billing/v1/me/merchant/customers", "/billing/v1/customers/x/balance", "/billing/v1/merchant/payment-providers",
		"/api/v1/merchants/host-selected/billing/me/checkout", "/api/v1/merchants/host-selected/billing/me/payment-methods",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/pay"+path, nil))
		require.Equal(t, 404, rec.Code, path)
	}
	routes, err := runtime.HTTPRoutes()
	require.NoError(t, err)
	actions := 0
	for _, route := range routes {
		if strings.HasPrefix(route.Path, "/api/v1/merchants/") && route.Method != http.MethodOptions {
			actions++
		}
	}
	require.Equal(t, 4, actions)
}

func TestCustomerExposureValidationRefusesAmbiguityAndFallbackAuthority(t *testing.T) {
	verifier := billingauth.DelegatedAuthenticatorFunc(reviewReject)
	for _, prefix := range []string{"", "/", "/customer/", "/customer/../other", "/customer/{tail...}", "/customer/{slug}/%2f"} {
		runtime := reviewRuntime(nil, verifier)
		require.Error(t, runtime.ConfigureHTTP(HTTPConfig{CustomerExposures: []CustomerHTTPConfig{{Prefix: prefix, DelegatedAuthenticator: verifier}}}), prefix)
	}
	runtime := reviewRuntime(nil, verifier)
	require.ErrorContains(t, runtime.ConfigureHTTP(HTTPConfig{CustomerExposures: []CustomerHTTPConfig{{Prefix: "/portal"}}}), "own authenticator")
	require.NoError(t, runtime.ConfigureHTTP(HTTPConfig{Customer: true, CustomerExposures: []CustomerHTTPConfig{{Prefix: "/v1/me", DelegatedAuthenticator: verifier}}}))
	_, err := runtime.HTTPRoutes()
	require.ErrorContains(t, err, "conflicting")
	standalone := reviewRuntime(nil, nil)
	require.NoError(t, standalone.ConfigureHTTP(HTTPConfig{Standalone: true}))
	_, err = standalone.HTTPRoutes()
	require.ErrorContains(t, err, "no control plane")
}

func TestCustomerBillingManagementRoutesAndCapabilities(t *testing.T) {
	runtime := reviewRuntime(nil, nil)
	require.NoError(t, runtime.ConfigureHTTP(HTTPConfig{CustomerExposures: []CustomerHTTPConfig{{
		Prefix: "/v1/me", Scope: CustomerBillingManagement,
		DelegatedAuthenticator: billingauth.DelegatedAuthenticatorFunc(reviewReject),
	}}}))
	mux := reviewMount(t, runtime)
	for _, path := range []string{"/products", "/payments", "/invoices", "/subscriptions", "/payment-methods"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/pay/v1/me"+path, nil))
		require.Equal(t, http.StatusUnauthorized, rec.Code, path)
	}
	for _, path := range []string{"/checkout", "/subscriptions/x/change-tier", "/subscriptions/x/provider-cutover", "/billing-portal", "/subscriptions/x/solana-tier-change"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/pay/v1/me"+path, nil))
		require.Equal(t, http.StatusNotFound, rec.Code, path)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/pay/v1/capabilities", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var capabilities struct {
		RouteGroups map[string]bool `json:"route_groups"`
		Features    map[string]bool `json:"features"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &capabilities))
	require.True(t, capabilities.RouteGroups["customer"])
	require.False(t, capabilities.RouteGroups["checkout"])
	require.False(t, capabilities.Features["stripe_billing_portal"])
	require.False(t, capabilities.Features["solana_one_time_payments"])
	require.False(t, capabilities.Features["provider_credential_writes"])
	require.NotContains(t, capabilities.Features, "webhooks")
	require.NotContains(t, rec.Body.String(), `"routes"`)
}
