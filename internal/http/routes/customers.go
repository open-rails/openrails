package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// customersRoutes is the merchant's customers: the record, its billing
// profile and policy, and who is overdue.
var customersRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/customers", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(queryOf(billing.CustomerListParams{}), text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.Customer]{}}},
		Errors: codes("invalid_cursor"), Handler: h(handlers.ListCustomers)},
	{Method: POST, Path: "/v1/merchant/customers/lookup", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Request: billing.CustomerLookupParams{}, Responses: []Reply{{200, billing.CustomerLookup{}}}, Errors: codes("invalid_param", billing.CodeServiceCredentialCustomerScopeDenied), Handler: h(handlers.GetCustomers)},
	{Method: POST, Path: "/v1/merchant/customers/ensure", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate,
		Request: billing.EnsureCustomerBatchParams{}, Responses: []Reply{{200, billing.EnsureCustomerBatchResult{}}}, Errors: codes("invalid_param", billing.CodeServiceCredentialCustomerScopeDenied), Handler: h(handlers.EnsureCustomers)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/billing-profile", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Responses: []Reply{{200, billing.CustomerBillingProfile{}}}, Errors: withCustomer("customer_not_found"), Handler: h(handlers.GetCustomerBillingProfile)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/billing-policy", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Responses: []Reply{{200, billing.CustomerBillingPolicy{}}}, Errors: withCustomer("customer_not_found"), Handler: h(handlers.GetCustomerBillingPolicy)},
	{Method: PUT, Path: "/v1/merchant/customers/{customer_id}/billing-policy", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate,
		Request: billing.SetCustomerBillingPolicyParams{}, Responses: []Reply{{200, billing.CustomerBillingPolicy{}}},
		Errors: withCustomer("billing_policy_not_found", "customer_not_found", "invalid_param"), Handler: h(handlers.SetCustomerBillingPolicy)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/delinquency", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Responses: []Reply{{200, billing.ListPage[billing.Delinquency]{}}}, Errors: withCustomer(), Handler: h(handlers.ListCustomerDelinquency)},
	{Method: GET, Path: "/v1/merchant/delinquency", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(queryOf(billing.DelinquencyListParams{}), text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.Delinquency]{}}},
		Errors: codes("invalid_cursor", "invalid_param"), Handler: h(handlers.ListDelinquency)},
}
