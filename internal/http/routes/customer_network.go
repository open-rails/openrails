package routes

import (
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// networkLinkErrors are what redeeming a link challenge answers.
var networkLinkErrors = codes(billing.CodeNetworkLinkChallengeInvalid, billing.CodeNetworkLinkConflict, billing.CodeNetworkAccountLinked,
	billing.CodeNetworkTermsRequired, "service_unavailable")

// customerNetworkRoutes link a merchant's customer to its openrails.dev
// account, where the deployment runs the customer network: the customer
// asks for a challenge, answers it with a network sign-in, and redeems it,
// with its own token or, on the checkout host, its order's checkout URL.
var customerNetworkRoutes = []Route{
	{Method: POST, Path: "/v1/me/network-link/challenges", Group: Customer, Auth: AuthCustomer, When: FeatureCustomerNetwork, Checkout: CheckoutLink,
		Responses: []Reply{{201, billing.NetworkLinkChallenge{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.CreateNetworkLinkChallenge)},
	{Method: POST, Path: "/v1/me/network-link", Group: Customer, Auth: AuthCustomer, When: FeatureCustomerNetwork, Checkout: CheckoutLink,
		Request: billing.CreateNetworkLinkParams{}, Responses: []Reply{{200, billing.CustomerLink{}}, {201, billing.CustomerLink{}}},
		Errors: networkLinkErrors, Handler: h(handlers.CreateNetworkLink)},
	{Method: GET, Path: "/v1/app/customers", Group: App, Auth: AuthApplication, Name: "ListNetworkCustomers", Permission: needCustomers, When: FeatureCustomerNetwork,
		Query: params(text("network_account_id")), Responses: []Reply{{200, billing.ListPage[billing.Customer]{}}},
		Errors: codes("service_unavailable"), Handler: h(handlers.ListNetworkCustomers)},
}

// linkPrefix is where the account acts through one of its links, as that
// link's customer.
const linkPrefix = "/v1/network/links/{link_id}"

// networkRoutes are the openrails.dev account's own API across its linked
// merchants (the network portal's): its links, subscriptions and orders, and
// through one link the customer routes it acts by as that merchant's
// customer, each the same operation /v1/me runs. The link is the grant: no
// path names a merchant.
func networkRoutes() []Route {
	out := []Route{
		{Method: GET, Path: "/v1/network/links", Group: Network, Auth: AuthNetwork, When: FeatureCustomerNetwork, NoConn: true,
			Query: pageParams, Responses: []Reply{{200, billing.ListPage[billing.NetworkLink]{}}}, Errors: codes("invalid_cursor", "service_unavailable"), Handler: h(handlers.ListNetworkLinks)},
		{Method: GET, Path: "/v1/network/subscriptions", Group: Network, Auth: AuthNetwork, When: FeatureCustomerNetwork, NoConn: true,
			Query: pageParams, Responses: []Reply{{200, billing.ListPage[billing.NetworkSubscription]{}}}, Errors: codes("invalid_cursor", "service_unavailable"), Handler: h(handlers.ListNetworkSubscriptions)},
		{Method: GET, Path: "/v1/network/orders", Group: Network, Auth: AuthNetwork, When: FeatureCustomerNetwork, NoConn: true,
			Query: pageParams, Responses: []Reply{{200, billing.ListPage[billing.NetworkOrder]{}}}, Errors: codes("invalid_cursor", "service_unavailable"), Handler: h(handlers.ListNetworkOrders)},
		{Method: DELETE, Path: "/v1/network/links/{link_id}", Group: Network, Auth: AuthNetwork, When: FeatureCustomerNetwork, NoConn: true, Sensitive: true,
			Responses: []Reply{{204, nil}}, Errors: codes(billing.CodeNetworkLinkInUse, "invalid_param", "service_unavailable"), Handler: h(handlers.UnlinkNetworkLink)},
		{Method: POST, Path: "/v1/network/links/{link_id}/revoke-authority", Group: Network, Auth: AuthNetwork, When: FeatureCustomerNetwork, Sensitive: true,
			Responses: []Reply{{200, billing.PaymentAuthorityRevocation{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.RevokePaymentAuthority)},
	}
	// The customer routes the account acts by through one link.
	for _, at := range []struct {
		key       string
		sensitive bool
	}{
		{"GET /v1/me/subscriptions/{id}", false},
		{"POST /v1/me/subscriptions/{id}/cancel", true},
		{"POST /v1/me/subscriptions/{id}/resume", true},
		{"PUT /v1/me/subscriptions/{id}/payment-method", true},
		{"GET /v1/me/payment-methods", false},
		{"POST /v1/me/payment-methods", true},
		{"DELETE /v1/me/payment-methods/{id}", true},
		{"GET /v1/me/orders/{id}", false},
		{"POST /v1/me/orders/{id}/pay", true},
		{"POST /v1/me/orders/{id}/confirm", true},
	} {
		out = append(out, throughLink(at.key, at.sensitive))
	}
	return out
}

// throughLink is the customer route key through one of the account's links:
// the same operation under /v1/network/links/{link_id}.
func throughLink(key string, sensitive bool) Route {
	for _, list := range [][]Route{subscriptionsRoutes, paymentMethodsRoutes, ordersRoutes} {
		for _, r := range list {
			if r.Key() != key {
				continue
			}
			r.Path = linkPrefix + strings.TrimPrefix(r.Path, "/v1/me")
			r.Group, r.Auth, r.When, r.Sensitive, r.Checkout = Network, AuthNetwork, FeatureCustomerNetwork, sensitive, ""
			return r
		}
	}
	panic("routes: no customer route " + key)
}
