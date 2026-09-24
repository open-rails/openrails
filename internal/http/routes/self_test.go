package routes

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

const handlerReached = -1

// reach reports the status, or handlerReached when the runtime-less handler
// ran past every gate and panicked.
func reach(h http.Handler, method, path string, header map[string]string) (code int) {
	defer func() {
		if recover() != nil {
			code = handlerReached
		}
	}()
	if header == nil {
		header = map[string]string{}
	}
	header["Authorization"] = "Bearer host-credential"
	return do(h, method, path, header).Code
}

func passedGates(code int) bool {
	return !slices.Contains([]int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed}, code)
}

func customerAuth(invoker string, perms ...string) router.Middleware {
	return middleware.DelegatedPrincipalRequired(hostDelegated(&billingauth.DelegatedPrincipal{
		MerchantID: merchantA.String(), MerchantSlug: "acme", SubjectID: userA, Invoker: invoker, Permissions: perms,
	}, nil))
}

func customerSurface(auth router.Middleware) http.Handler {
	mux := http.NewServeMux()
	RegisterSelfServiceRoutes(router.NewMux(mux, "/v1/me", nil), nil, auth, routesurface.AllProviderRoutes())
	RegisterCustomerTreasuryRoutes(router.NewMux(mux, "/v1/customers", nil), nil, auth, routesurface.AllProviderRoutes())
	return mux
}

// /me routes act only on the authenticated subject and need no extra grant;
// an invoker-scoped credential may read only its own spend windows.
func TestSelfServiceAuthorization(t *testing.T) {
	payer := customerSurface(customerAuth(""))
	for _, route := range []string{
		"GET /v1/me/balance", "GET /v1/me/transactions", "GET /v1/me/tier?group=premium", "GET /v1/me/spend-limits",
		"PUT /v1/me/collection-payment-method", "POST /v1/me/subscriptions/sub_1/cancel", "POST /v1/me/subscriptions/sub_1/resume",
		"PUT /v1/me/subscriptions/sub_1/payment-method", "POST /v1/me/subscriptions/sub_1/change-tier",
		"POST /v1/me/subscriptions/sub_1/solana-cancel-tx", "POST /v1/me/checkout",
	} {
		method, path, _ := strings.Cut(route, " ")
		code := reach(payer, method, path, nil)
		require.True(t, passedGates(code), "%s: %d", route, code)
	}

	invoker := customerSurface(customerAuth("end-user-7", permissions.CustomerAll))
	require.True(t, passedGates(reach(invoker, http.MethodGet, "/v1/me/spend-limits", nil)))
	for _, route := range []string{"GET /v1/me/balance", "POST /v1/me/subscriptions/sub_1/cancel", "GET /v1/me/payment-methods", "POST /v1/me/checkout", "GET /v1/customers/" + userA + "/balance"} {
		method, path, _ := strings.Cut(route, " ")
		require.Equal(t, http.StatusForbidden, reach(invoker, method, path, nil), route)
	}

	anonymous := customerSurface(middleware.DelegatedPrincipalRequired(hostDelegated(nil, billingauth.ErrUnauthenticated)))
	require.Equal(t, http.StatusUnauthorized, reach(anonymous, http.MethodGet, "/v1/me/balance", nil))
	require.Equal(t, http.StatusUnauthorized, reach(anonymous, http.MethodGet, "/v1/me/spend-limits", nil))
	require.Equal(t, http.StatusConflict, reach(payer, http.MethodGet, "/v1/me/balance", map[string]string{merchant.BindingHeader: merchantB.String()}),
		"a browser cannot select a merchant other than its verified one")
}

// /customers/:customer_id binds only the caller's own payable subject, or the
// merchant's own account for a merchant administrator, and every route needs
// its customer:* grant.
func TestCustomerTreasuryAuthorization(t *testing.T) {
	path := func(customer string) string { return "/v1/customers/" + customer + "/spend-delegations" }
	read, update := permissions.CustomerSpendDelegationsRead, permissions.CustomerSpendDelegationsUpdate
	for _, tc := range []struct {
		name, method, customer string
		perms                  []string
		reached                bool
	}{
		{"own subject with grant", http.MethodGet, userA, []string{read}, true},
		{"own subject without grant", http.MethodGet, userA, nil, false},
		{"read grant cannot update", http.MethodPut, userA, []string{read}, false},
		{"update grant", http.MethodPut, userA, []string{update}, true},
		{"another customer", http.MethodGet, userB, []string{permissions.MerchantAll, read}, false},
		{"merchant account needs merchant administrator", http.MethodGet, "acme", []string{read}, false},
		{"merchant administration is not a customer grant", http.MethodGet, "acme", []string{permissions.MerchantAll}, false},
		{"merchant administrator on merchant slug", http.MethodGet, "acme", []string{permissions.MerchantAll, read}, true},
		{"merchant administrator on merchant id", http.MethodGet, merchantA.String(), []string{permissions.MerchantAll, read}, true},
		{"merchant administrator on another merchant", http.MethodGet, merchantB.String(), []string{permissions.MerchantAll, read}, false},
	} {
		code := reach(customerSurface(customerAuth("", tc.perms...)), tc.method, path(tc.customer), nil)
		if tc.reached {
			require.True(t, passedGates(code), "%s: %d", tc.name, code)
		} else {
			require.Equal(t, http.StatusForbidden, code, tc.name)
		}
	}
}

func TestCustomerRouteInventories(t *testing.T) {
	collect := func(register func(router.Router)) []string {
		table := &router.Table{}
		register(router.NewMux(table, "/me", nil))
		return routeKeys(table)
	}
	auth := customerAuth("")
	all := routesurface.AllProviderRoutes()
	full := collect(func(r router.Router) { RegisterSelfServiceRoutes(r, nil, auth, all) })
	management := collect(func(r router.Router) { RegisterCustomerBillingManagementRoutes(r, nil, auth, all) })
	require.Subset(t, full, management)
	var purchaseOnly []string
	for _, key := range full {
		if !slices.Contains(management, key) {
			purchaseOnly = append(purchaseOnly, key)
		}
	}
	require.ElementsMatch(t, []string{
		"POST /me/checkout", "POST /me/billing-portal",
		"POST /me/subscriptions/{id}/change-tier", "POST /me/subscriptions/{id}/change-tier/preview",
		"POST /me/subscriptions/{id}/provider-cutover", "GET /me/subscriptions/{id}/provider-cutover", "POST /me/subscriptions/{id}/provider-cutover/preview",
		"POST /me/subscriptions/{id}/solana-tier-change", "POST /me/subscriptions/{id}/solana-tier-change/confirm",
	}, purchaseOnly, "management scope never purchases or changes plans")
	require.Subset(t, management, []string{"GET /me/checkout/{id}", "POST /me/checkout/{id}/confirm", "POST /me/subscriptions/{id}/solana-cancel", "GET /me/spend-limits"})

	require.ElementsMatch(t, []string{
		"PUT /me/collection-payment-method", "POST /me/subscriptions/{id}/cancel", "POST /me/subscriptions/{id}/resume", "PUT /me/subscriptions/{id}/payment-method",
	}, collect(func(r router.Router) { RegisterCustomerSubscriptionManagementRoutes(r, nil, auth) }))

	for _, tc := range []struct {
		providers         routesurface.ProviderRoutes
		portal, solanaTxn bool
	}{
		{routesurface.ProviderRoutes{}, false, false},
		{routesurface.ProviderRoutes{StripePortal: true}, true, false},
		{routesurface.ProviderRoutes{Solana: true}, false, false},
		{routesurface.ProviderRoutes{SolanaSigning: true}, false, true},
	} {
		self := collect(func(r router.Router) { RegisterSelfServiceRoutes(r, nil, auth, tc.providers) })
		require.Equal(t, tc.portal, slices.Contains(self, "POST /me/billing-portal"), "%+v", tc.providers)
		require.Equal(t, tc.solanaTxn, slices.Contains(self, "POST /me/subscriptions/{id}/solana-cancel-tx"), "%+v", tc.providers)
		treasury := collect(func(r router.Router) { RegisterCustomerTreasuryRoutes(r, nil, auth, tc.providers) })
		require.Equal(t, tc.portal, slices.Contains(treasury, "POST /me/{customer_id}/billing-portal"), "%+v", tc.providers)
		require.NotContains(t, treasury, "GET /me/{customer_id}/status", "the treasury does not report consumer state")
	}
}
