package routes

import (
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
	{Method: GET, Path: "/v1/app/customers", Group: App, Auth: AuthApplication, Name: "ListNetworkCustomers", Permission: NeedCustomers, When: FeatureCustomerNetwork,
		Query: params(text("network_account_id")), Responses: []Reply{{200, billing.ListPage[billing.Customer]{}}},
		Errors: codes("service_unavailable"), Handler: h(handlers.ListNetworkCustomers)},
}
