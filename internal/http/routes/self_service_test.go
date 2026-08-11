package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	merchantpkg "github.com/open-rails/openrails/pkg/merchant"
)

// These tests pin the SELF-SERVICE route table (RegisterSelfServiceRoutes) for
// the browser-direct `/v1/me/*` surface that host apps call with delegated
// tokens (issues #215/#216 consumer gap; neutral registration since #670).
//
// The assertions exercise the per-route permission gates, which run BEFORE the
// wrapped handler — so no Runtime/services are needed: a denied request (403) or
// an unauthenticated one (401) never reaches the handler body. A route that was
// NOT mounted would 404 instead, which is exactly the regression we guard.

// fakeDelegatedResolver implements middleware.DelegatedResolver: it returns a
// fixed ResolvedDelegated carrying the supplied permission set.
type fakeDelegatedResolver struct {
	permissions []string
	merchantID  merchantpkg.ID
	err         error
}

func (f fakeDelegatedResolver) ResolveDelegated(context.Context, string, string) (*controlplane.ResolvedDelegated, error) {
	if f.err != nil {
		return nil, f.err
	}
	merchantID := f.merchantID
	if merchantID.IsZero() {
		merchantID = dbtest.TestMerchantID
	}
	return &controlplane.ResolvedDelegated{
		Merchant:         "acme-merchant",
		MerchantID:       merchantID,
		MerchantSlug:     "acme-merchant",
		DelegatedSubject: "user-42",
		Permissions:      f.permissions,
	}, nil
}

func newSelfRouter(t *testing.T, perms []string) http.Handler {
	t.Helper()
	return newSelfRouterWithResolver(t, fakeDelegatedResolver{permissions: perms})
}

func newSelfRouterWithResolver(t *testing.T, resolver middleware.DelegatedResolver) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	// rt==nil: MerchantDBConn is skipped, and the wrapped handlers are never
	// reached on the 401/403 paths these tests assert.
	RegisterSelfServiceRoutes(router.NewMux(mux, "/v1/me", nil), nil,
		middleware.DelegatedSelfRequired(resolver), routesurface.AllProviderRoutes())
	return mux
}

func newSelfRouterWithProviderRoutes(t *testing.T, providerRoutes routesurface.ProviderRoutes) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	RegisterSelfServiceRoutes(router.NewMux(mux, "/v1/me", nil), nil,
		middleware.DelegatedSelfRequired(fakeDelegatedResolver{}), providerRoutes)
	return mux
}

func newCustomerTreasuryRouter(t *testing.T, perms []string) http.Handler {
	t.Helper()
	return newCustomerTreasuryRouterWithProviderRoutes(t, routesurface.AllProviderRoutes(), perms)
}

func newCustomerTreasuryRouterWithProviderRoutes(t *testing.T, providerRoutes routesurface.ProviderRoutes, perms []string) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	RegisterCustomerTreasuryRoutes(router.NewMux(mux, "/v1/customers", nil), nil,
		middleware.DelegatedSelfRequired(fakeDelegatedResolver{permissions: perms}), providerRoutes)
	return mux
}

func doSelf(e http.Handler, method, path string, withAuth bool) *httptest.ResponseRecorder {
	token := ""
	if withAuth {
		token = "delegated.jwt.token"
	}
	return doSelfBearer(e, method, path, token)
}

func doSelfBearer(e http.Handler, method, path, token string) *httptest.ResponseRecorder {
	return doSelfBearerBody(e, method, path, token, "{}")
}

func doSelfBearerBody(e http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

func TestSelfService_PermissionlessPrincipalReachesMountedSelfRoutes(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"resume", http.MethodPost, "/v1/me/subscriptions/sub_123/resume"},
		{"change-tier", http.MethodPost, "/v1/me/subscriptions/sub_123/change-tier"},
		{"update-payment-method", http.MethodPut, "/v1/me/subscriptions/sub_123/payment-method"},
		{"cancel", http.MethodPost, "/v1/me/subscriptions/sub_123/cancel"},
		{"solana-cancel-tx", http.MethodPost, "/v1/me/subscriptions/sub_123/solana-cancel-tx"},
		{"solana-cancel-confirm", http.MethodPost, "/v1/me/subscriptions/sub_123/solana-cancel"},
		{"solana-tier-change", http.MethodPost, "/v1/me/subscriptions/sub_123/solana-tier-change"},
		{"solana-tier-change-confirm", http.MethodPost, "/v1/me/subscriptions/sub_123/solana-tier-change/confirm"},
		{"balance", http.MethodGet, "/v1/me/balance"},
		{"tier", http.MethodGet, "/v1/me/tier?group=premium"},
		{"transactions", http.MethodGet, "/v1/me/transactions"},
		{"settings", http.MethodPut, "/v1/me/settings"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newSelfRouter(t, nil)
			func() {
				defer func() { _ = recover() }()
				w := doSelf(e, tc.method, tc.path, true)
				require.NotEqual(t, http.StatusUnauthorized, w.Code, "%s authenticated principal rejected", tc.name)
				require.NotEqual(t, http.StatusForbidden, w.Code, "%s must not require self permissions", tc.name)
				require.NotEqual(t, http.StatusNotFound, w.Code, "%s must be mounted", tc.name)
			}()
		})
	}
}

func TestSelfServiceProviderRoutesAreConditional(t *testing.T) {
	none := newSelfRouterWithProviderRoutes(t, routesurface.ProviderRoutes{})
	require.Equal(t, http.StatusNotFound, doSelf(none, http.MethodPost, "/v1/me/billing-portal", true).Code)
	require.Equal(t, http.StatusNotFound, doSelf(none, http.MethodPost, "/v1/me/subscriptions/sub_123/solana-cancel-tx", true).Code)
	require.Equal(t, http.StatusNotFound, doSelf(none, http.MethodPost, "/v1/me/stripe/portal", true).Code)

	stripe := newSelfRouterWithProviderRoutes(t, routesurface.ProviderRoutes{StripePortal: true})
	func() {
		defer func() { _ = recover() }()
		w := doSelf(stripe, http.MethodPost, "/v1/me/billing-portal", true)
		require.NotEqual(t, http.StatusNotFound, w.Code)
	}()

	// Solana SIGNING routes (cancel/tier) gate on SolanaSigning, not Solana (#661):
	// one-off config alone does not mount them.
	solanaConfigOnly := newSelfRouterWithProviderRoutes(t, routesurface.ProviderRoutes{Solana: true})
	require.Equal(t, http.StatusNotFound, doSelf(solanaConfigOnly, http.MethodPost, "/v1/me/subscriptions/sub_123/solana-cancel-tx", true).Code)

	solana := newSelfRouterWithProviderRoutes(t, routesurface.ProviderRoutes{Solana: true, SolanaSigning: true})
	func() {
		defer func() { _ = recover() }()
		w := doSelf(solana, http.MethodPost, "/v1/me/subscriptions/sub_123/solana-cancel-tx", true)
		require.NotEqual(t, http.StatusNotFound, w.Code)
	}()
}

func TestCustomerTreasuryBillingPortalRouteIsConditional(t *testing.T) {
	perms := []string{controlplane.PermCustomerPaymentMethodsUpdate}

	none := newCustomerTreasuryRouterWithProviderRoutes(t, routesurface.ProviderRoutes{}, perms)
	require.Equal(t, http.StatusNotFound, doSelf(none, http.MethodPost, "/v1/customers/cus_123/billing-portal", true).Code)
	require.Equal(t, http.StatusNotFound, doSelf(none, http.MethodPost, "/v1/customers/cus_123/stripe/portal", true).Code)

	stripe := newCustomerTreasuryRouterWithProviderRoutes(t, routesurface.ProviderRoutes{StripePortal: true}, perms)
	func() {
		defer func() { _ = recover() }()
		w := doSelf(stripe, http.MethodPost, "/v1/customers/cus_123/billing-portal", true)
		require.NotEqual(t, http.StatusNotFound, w.Code)
	}()
}

// No token at all is rejected by the delegated auth middleware before any gate.
func TestSelfService_ResumeRejectedWithoutToken(t *testing.T) {
	e := newSelfRouter(t, nil)
	w := doSelf(e, http.MethodPost, "/v1/me/subscriptions/sub_123/resume", false)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestSelfService_RejectsServiceCredential(t *testing.T) {
	e := newSelfRouterWithResolver(t, fakeDelegatedResolver{err: controlplane.ErrDelegatedInvalid})
	w := doSelfBearer(e, http.MethodPost, "/v1/me/subscriptions/sub_123/resume", "openrails_st_keyid_secret")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "delegated_token_invalid")
}

func TestCustomerTreasurySpendDelegationsMountedAndGated(t *testing.T) {
	e := newCustomerTreasuryRouter(t, nil)
	w := doSelf(e, http.MethodGet, "/v1/customers/acme-merchant/spend-delegations", false)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	e = newCustomerTreasuryRouter(t, []string{controlplane.PermMerchantCustomerSettingsRead})
	w = doSelf(e, http.MethodGet, "/v1/customers/acme-merchant/spend-delegations", true)
	require.Equal(t, http.StatusForbidden, w.Code, "merchant:* must not satisfy customer treasury")

	e = newCustomerTreasuryRouter(t, []string{controlplane.PermCustomerSpendDelegationsRead})
	w = doSelf(e, http.MethodGet, "/v1/customers/other-customer/spend-delegations", true)
	require.Equal(t, http.StatusForbidden, w.Code, "caller must be scoped to the customer in the path")

	w = doSelf(e, http.MethodGet, "/v1/customers/acme-merchant/spend-delegations", true)
	require.NotEqual(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())
	require.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())

	w = doSelf(e, http.MethodPut, "/v1/customers/acme-merchant/spend-delegations", true)
	require.Equal(t, http.StatusForbidden, w.Code, "read permission must not satisfy update")
	w = doSelf(e, http.MethodPut, "/v1/customers/acme-merchant/spend-delegations:upsert", true)
	require.Equal(t, http.StatusForbidden, w.Code, "read permission must not satisfy single-row update")
}

func TestCustomerTreasurySpendDelegationsRejectBodyCustomerID(t *testing.T) {
	e := newCustomerTreasuryRouter(t, []string{controlplane.PermCustomerSpendDelegationsUpdate})
	body := `{
		"customer_id": "11111111-1111-1111-1111-111111111111",
		"delegations": [{
			"scope": "invoker",
			"scope_key": "22222222-2222-2222-2222-222222222222",
			"windows": [{"key": "day", "window_seconds": 86400, "limit": 1000, "currency": "USD"}]
		}]
	}`
	w := doSelfBearerBody(e, http.MethodPut, "/v1/customers/acme-merchant/spend-delegations", "delegated.jwt.token", body)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "customer_id")

	body = `{
		"delegations": [{
			"scope": "invoker",
			"scope_key": "22222222-2222-2222-2222-222222222222",
			"customer_id": "11111111-1111-1111-1111-111111111111",
			"windows": [{"key": "day", "window_seconds": 86400, "limit": 1000, "currency": "USD"}]
		}]
	}`
	w = doSelfBearerBody(e, http.MethodPut, "/v1/customers/acme-merchant/spend-delegations", "delegated.jwt.token", body)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "customer_id")

	for _, body := range []string{
		`{"scope":"invoker","windows":[{"key":"day","window_seconds":86400,"limit":1000,"currency":"USD"}]}`,
		`{"scope":"invoker","scope_key":"invoker-1","windows":[]}`,
		`{"scope":"invoker","scope_key":"invoker-1","windows":[{"key":"day","window_seconds":86400,"limit":1000,"currency":"BTC"}]}`,
	} {
		w = doSelfBearerBody(e, http.MethodPut, "/v1/customers/acme-merchant/spend-delegations:upsert", "delegated.jwt.token", body)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	}

	// or#893 phase 7: role_id is gone. A document that still sends it is
	// refused with the rewrite — never silently dropped into an unaddressed
	// delegation.
	for _, path := range []string{
		"/v1/customers/acme-merchant/spend-delegations",
		"/v1/customers/acme-merchant/spend-delegations:upsert",
	} {
		body := `{"scope":"role","role_id":"22222222-2222-2222-2222-222222222222","windows":[{"key":"day","window_seconds":86400,"limit":1000,"currency":"USD"}]}`
		if !strings.HasSuffix(path, ":upsert") {
			body = `{"delegations":[` + body + `]}`
		}
		w = doSelfBearerBody(e, http.MethodPut, path, "delegated.jwt.token", body)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), `role_id was removed (or#893): address a role delegation as {\"scope\":\"role\",\"scope_key\":\"\u003crole uuid\u003e\"}`)
	}
}
