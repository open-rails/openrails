package routes

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/merchant"
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
	return do(h, method, path, header).Code
}

func passedGates(code int) bool {
	return !slices.Contains([]int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed}, code)
}

func customerSurface(a billingauth.Auth) http.Handler {
	mux := http.NewServeMux()
	rt := &app.Runtime{}
	rt.SetConfiguredMerchant(merchantA)
	RegisterCustomerRoutes(router.NewMux(mux, "/v1/me", rt), rt, CustomerMount{Auth: a, Providers: routesurface.AllProviderRoutes()})
	return mux
}

// /me routes act only on the admitted subject and need no grant; an invoker
// acting for the subject may read only its own spend limits.
func TestSelfServiceAuthorization(t *testing.T) {
	fake := &authtest.Fake{}
	payer := customerSurface(fake)
	token := fake.Person(userA)
	auth := map[string]string{"Authorization": "Bearer " + token}
	for _, route := range []string{
		"GET /v1/me", "GET /v1/me/balance/transactions", "GET /v1/me/spend-limits",
		"PUT /v1/me/collection-payment-method", "POST /v1/me/subscriptions/sub_1/cancel", "POST /v1/me/subscriptions/sub_1/resume",
		"PUT /v1/me/subscriptions/sub_1/payment-method", "POST /v1/me/subscriptions/sub_1/change-tier",
	} {
		method, path, _ := strings.Cut(route, " ")
		code := reach(payer, method, path, auth)
		require.True(t, passedGates(code), "%s: %d", route, code)
	}

	delegated := authtest.Application(userB)
	delegated.Invoker = billingauth.Invoker{Issuer: "https://cozy.example", ID: "u_42"}
	invoker := map[string]string{"Authorization": "Bearer " + fake.Issue(authtest.Grant{Identity: delegated})}
	require.True(t, passedGates(reach(payer, http.MethodGet, "/v1/me/spend-limits", invoker)))
	for _, route := range []string{"GET /v1/me", "POST /v1/me/subscriptions/sub_1/cancel", "GET /v1/me/payment-methods", "POST /v1/me/checkout-sessions"} {
		method, path, _ := strings.Cut(route, " ")
		require.Equal(t, http.StatusForbidden, reach(payer, method, path, invoker), route)
	}

	require.Equal(t, http.StatusUnauthorized, reach(payer, http.MethodGet, "/v1/me", map[string]string{"Authorization": "Bearer forged"}))
	require.Equal(t, http.StatusUnauthorized, reach(payer, http.MethodGet, "/v1/me/spend-limits", map[string]string{"Authorization": "Bearer forged"}))
	require.Equal(t, http.StatusConflict, reach(payer, http.MethodGet, "/v1/me", map[string]string{"Authorization": "Bearer " + token, merchant.SelectorHeader: "id:" + merchantB.String()}),
		"a browser cannot select a merchant other than the mount's")
}

func TestCustomerRouteInventories(t *testing.T) {
	collect := func(register func(router.Router)) []string {
		table := &router.Table{}
		register(router.NewMux(table, "/me", nil))
		return routeKeys(table)
	}
	all := routesurface.AllProviderRoutes()
	mount := func(providers routesurface.ProviderRoutes) CustomerMount {
		return CustomerMount{Auth: authtest.Deny{}, Providers: providers}
	}
	require.Subset(t, collect(func(r router.Router) { RegisterCustomerRoutes(r, nil, mount(all)) }), []string{
		"POST /me/checkout-sessions", "POST /me/billing-portal", "GET /me/spend-limits",
		"POST /me/subscriptions/{id}/change-tier", "POST /me/subscriptions/{id}/cancel", "PUT /me/collection-payment-method",
	})

	for _, tc := range []struct {
		providers routesurface.ProviderRoutes
		portal    bool
	}{
		{routesurface.ProviderRoutes{}, false},
		{routesurface.ProviderRoutes{StripePortal: true}, true},
		{routesurface.ProviderRoutes{Solana: true}, false},
		{routesurface.ProviderRoutes{SolanaSigning: true}, false},
	} {
		self := collect(func(r router.Router) { RegisterCustomerRoutes(r, nil, mount(tc.providers)) })
		require.Equal(t, tc.portal, slices.Contains(self, "POST /me/billing-portal"), "%+v", tc.providers)
	}
}
