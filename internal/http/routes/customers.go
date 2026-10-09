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
		Query: params(queryOf(billing.CustomerListParams{}), idsParam, text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.Customer]{}}},
		Errors: codes("invalid_cursor", "service_unavailable"), Handler: h(handlers.ListCustomers)},
	{Method: GET, Path: "/v1/admin/customers/{customer_id}", Group: Admin, Auth: AuthMerchant, Name: "GetCustomer", Level: LevelRead,
		Responses: []Reply{{200, billing.Customer{}}}, Errors: withCustomer("customer_not_found", "service_unavailable"), Handler: h(handlers.GetCustomer)},
	{Method: PATCH, Path: "/v1/admin/customers/{customer_id}", Group: Admin, Auth: AuthMerchant, Name: "UpdateCustomer", Level: LevelUpdate, Sensitive: true, Limit: middleware.AdminOperationGrant,
		Request: billing.UpdateCustomerParams{}, Responses: []Reply{{200, billing.Customer{}}},
		Errors: withCustomer("billing_policy_not_found", "currency_unsupported", "invalid_param", "service_unavailable"), Handler: h(handlers.UpdateCustomer)},
}
