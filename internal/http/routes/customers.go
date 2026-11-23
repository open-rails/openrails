package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

// customersRoutes is the merchant's customers: the record with its contact,
// settings and money, and its settings write.
var customersRoutes = []Route{
	{Method: GET, Path: "/v1/admin/customers", Group: Admin, Auth: AuthMerchant, Name: "ListCustomers", Level: LevelRead,
		Doc:   "ListCustomers lists the merchant's customers, newest first.",
		Query: billing.CustomerListParams{}, Responses: []Reply{{200, billing.ListPage[billing.Customer]{}}},
		Errors: codes("invalid_cursor", "service_unavailable"), Handler: h(handlers.ListCustomers)},
	{Method: GET, Path: "/v1/admin/customers/{customer_id}", Group: Admin, Auth: AuthMerchant, Name: "GetCustomer", Level: LevelRead,
		Args: []any{billing.CustomerID{}},
		Doc: "GetCustomer reads one customer: its contact, its settings, and per currency its balance and the card " +
			"that pays its invoices.",
		Responses: []Reply{{200, billing.Customer{}}}, Errors: withCustomer("customer_not_found", "service_unavailable"), Handler: h(handlers.GetCustomer)},
	{Method: PATCH, Path: "/v1/admin/customers/{customer_id}", Group: Admin, Auth: AuthMerchant, Name: "UpdateCustomer", Level: LevelUpdate, Limit: middleware.AdminOperationGrant,
		Args: []any{billing.CustomerID{}},
		Doc: "UpdateCustomer changes one customer's settings, only the fields params names, and returns its ID and " +
			"saved settings; a customer not billed yet is created. Its authority is the route's permission; no " +
			"recent sign-in is asked for.",
		Request: billing.UpdateCustomerParams{}, Responses: []Reply{{200, billing.CustomerSettingsResult{}}},
		Errors: withCustomer("billing_policy_not_found", "currency_unsupported", "invalid_param", billing.CodeLimitNotFound, "service_unavailable"), Handler: h(handlers.UpdateCustomer)},
}
