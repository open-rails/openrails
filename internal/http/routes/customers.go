package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// customersRoutes is the merchant's view of a customer: the list, the billing
// profile, the billing policy and whether the customer is overdue, and the
// customer's own status summary.
var customersRoutes = []Route{
	{Method: PUT, Path: "/v1/merchant/customers/{customer_id}", Group: MerchantAPI, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate,
		Responses: []Reply{{200, billing.Customer{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ServiceEnsureCustomer)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/billing-policy", Group: MerchantAPI, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Responses: []Reply{{200, billing.CustomerBillingPolicyAssignment{}}}, Errors: codes("customer_not_found", "invalid_param"), Handler: h(handlers.ServiceGetCustomerBillingPolicy)},
	{Method: PUT, Path: "/v1/merchant/customers/{customer_id}/billing-policy", Group: MerchantAPI, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate,
		Request: handlers.CustomerBillingPolicyWrite{}, Responses: []Reply{{200, billing.CustomerBillingPolicyAssignment{}}}, Errors: codes("billing_policy_not_found", "customer_not_found", "invalid_param"), Handler: h(handlers.ServiceSetCustomerBillingPolicy)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/delinquency", Group: MerchantAPI, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "invalid_param"), Handler: h(handlers.ServiceGetCustomerDelinquency)},
	{Method: GET, Path: "/v1/merchant/delinquency", Group: MerchantAPI, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(integer("limit"), text("state")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ServiceListDelinquency)},
	{Method: GET, Path: "/v1/merchant/customers", Group: MerchantAdmin, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(integer("limit"), integer("offset"), text("q")), Responses: []Reply{{200, PathPage[handlers.AdminCustomerSummary]{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ListAdminCustomers)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}", Group: MerchantAdmin, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("currency")), Responses: []Reply{{200, handlers.AdminUserBillingProfile{}}}, Errors: codes("catalog_scope_mismatch", "invalid_param"), Handler: h(handlers.GetAdminUserBillingProfile)},
	{Method: GET, Path: "/v1/me/status", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{200, billing.BillingStatus{}}}, Errors: codes("authentication_required", "catalog_scope_mismatch"), Handler: h(handlers.GetMyBillingStatus)},
}
