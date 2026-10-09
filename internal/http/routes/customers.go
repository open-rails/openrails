package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

// customersRoutes is the merchant's customers: the record, its settings and
// billing profile, and who is overdue.
var customersRoutes = []Route{
	{Method: GET, Path: "/v1/admin/customers", Group: Admin, Auth: AuthMerchant, Name: "ListCustomers", Level: LevelRead,
		Query: params(queryOf(billing.CustomerListParams{}), idsParam, text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.Customer]{}}},
		Errors: codes("invalid_cursor"), Handler: h(handlers.ListCustomers)},
	{Method: POST, Path: "/v1/admin/customers/ensure", Group: Admin, Auth: AuthMerchant, Name: "EnsureCustomers", Level: LevelWrite, Sensitive: true,
		Request: billing.EnsureCustomerBatchParams{}, Responses: []Reply{{200, billing.EnsureCustomerBatchResult{}}}, Errors: codes("invalid_param", billing.CodeServiceCredentialCustomerScopeDenied), Handler: h(handlers.EnsureCustomers)},
	{Method: GET, Path: "/v1/admin/customers/settings", Group: Admin, Auth: AuthMerchant, Name: "ListCustomerSettings", Level: LevelRead,
		Query: params(idsParam, text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.CustomerSettings]{}}},
		Errors: codes("invalid_cursor"), Handler: h(handlers.ListCustomerSettings)},
	{Method: PATCH, Path: "/v1/admin/customers/settings", Group: Admin, Auth: AuthMerchant, Name: "UpdateCustomerSettings", Level: LevelWrite, Sensitive: true, Limit: middleware.AdminOperationGrant,
		Request: billing.UpdateCustomerSettingsBatchParams{}, Responses: []Reply{{200, billing.CustomerSettingsBatch{}}},
		Errors: codes("billing_policy_not_found", "currency_unsupported", "customer_not_found", "invalid_param", billing.CodeServiceCredentialCustomerScopeDenied), Handler: h(handlers.UpdateCustomerSettings)},
	{Method: GET, Path: "/v1/admin/customers/{customer_id}/billing-profile", Group: Admin, Auth: AuthMerchant, Name: "GetCustomerBillingProfile", Level: LevelRead,
		Responses: []Reply{{200, billing.CustomerBillingProfile{}}}, Errors: withCustomer("customer_not_found"), Handler: h(handlers.GetCustomerBillingProfile)},
	{Method: GET, Path: "/v1/admin/customers/{customer_id}/delinquency", Group: Admin, Auth: AuthMerchant, Name: "ListCustomerDelinquency", Level: LevelRead,
		Responses: []Reply{{200, billing.ListPage[billing.Delinquency]{}}}, Errors: withCustomer(), Handler: h(handlers.ListCustomerDelinquency)},
	{Method: GET, Path: "/v1/admin/delinquency", Group: Admin, Auth: AuthMerchant, Name: "ListDelinquency", Level: LevelRead,
		Query: params(queryOf(billing.DelinquencyListParams{}), text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.Delinquency]{}}},
		Errors: codes("invalid_cursor", "invalid_param"), Handler: h(handlers.ListDelinquency)},
}
