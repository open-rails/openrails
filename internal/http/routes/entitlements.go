package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

// entitlementsRoutes is access: the products a customer holds, the keys those
// products grant them, and the products staff grant free.
var entitlementsRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/entitlements", Group: Merchant, Auth: AuthMerchant, Name: "ListCustomerEntitlements", Level: LevelRead, Resources: res(ResAccess),
		Query: params(text("at"), text("prefix"), pageParams), Responses: []Reply{{200, billing.ListPage[billing.CustomerEntitlement]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "service_credential_customer_scope_denied"), Handler: h(handlers.ServiceListCustomerEntitlements)},
	{Method: GET, Path: "/v1/merchant/entitlements/{entitlement}/customers", Group: Merchant, Auth: AuthMerchant, Name: "ListEntitlementCustomers", Level: LevelRead, Resources: res(ResAccess),
		Query: params(text("at"), pageParams), Responses: []Reply{{200, billing.ListPage[billing.CustomerID]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query"), Handler: h(handlers.ServiceListEntitlementCustomers)},
	{Method: POST, Path: "/v1/merchant/customers/{customer_id}/entitlements/check", Group: Merchant, Auth: AuthMerchant, Name: "CheckEntitlements", Level: LevelRead, Resources: res(ResAccess),
		Request: billing.CheckEntitlementsParams{}, Responses: []Reply{{200, billing.EntitlementCheck{}}}, Errors: codes("invalid_param", "service_credential_customer_scope_denied"), Handler: h(handlers.ServiceCheckEntitlements)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/tier", Group: Merchant, Auth: AuthMerchant, Name: "GetEffectiveTier", Level: LevelRead, Resources: res(ResAccess),
		Query: params(text("group")), Responses: []Reply{{200, billing.EffectiveTier{}}}, Errors: codes("invalid_param", "service_credential_customer_scope_denied"), Handler: h(handlers.ServiceGetEffectiveTier)},
	{Method: POST, Path: "/v1/merchant/customers/{customer_id}/product-access/check", Group: Merchant, Auth: AuthMerchant, Name: "CheckProductAccess", Level: LevelRead, Resources: res(ResAccess),
		Request: billing.CheckProductAccessParams{}, Responses: []Reply{{200, billing.ProductAccessCheck{}}}, Errors: codes("invalid_param", "service_credential_customer_scope_denied"), Handler: h(handlers.CheckProductAccess)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/product-access", Group: Merchant, Auth: AuthMerchant, Name: "ListProductAccess", Level: LevelRead, Resources: res(ResAccess),
		Query: params(Param{Name: "live", Kind: "boolean"}, pageParams), Responses: []Reply{{200, billing.ListPage[billing.ProductAccessGrant]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "service_credential_customer_scope_denied"), Handler: h(handlers.ListProductAccess)},
	{Method: POST, Path: "/v1/merchant/product-access", Group: Merchant, Auth: AuthMerchant, Name: "CreateProductAccess", Level: LevelWrite, Resources: res(ResAccess), Sensitive: true, Limit: middleware.AdminOperationGrant, IdempotencyKey: true,
		Request: billing.CreateProductAccessBatchParams{}, Responses: []Reply{{201, billing.CreateProductAccessBatchResult{}}}, Errors: codes("authentication_required", "idempotency_key_reused", "invalid_param", "resource_not_found", "service_credential_customer_scope_denied"), Handler: h(handlers.CreateProductAccess)},
	{Method: DELETE, Path: "/v1/merchant/customers/{customer_id}/product-access/{id}", Group: Merchant, Auth: AuthMerchant, Name: "DeleteProductAccess", Level: LevelWrite, Resources: res(ResAccess), Sensitive: true, Limit: middleware.AdminOperationDestructive,
		Responses: []Reply{{204, nil}}, Errors: codes("invalid_param", "resource_not_found", "service_credential_customer_scope_denied"), Handler: h(handlers.DeleteProductAccess)},
	{Method: GET, Path: "/v1/me/entitlements", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: params(text("at"), text("prefix"), pageParams), Responses: []Reply{{200, billing.ListPage[billing.CustomerEntitlement]{}}}, Errors: codes("authentication_required", "invalid_cursor", "invalid_param", "invalid_query"), Handler: h(handlers.SelfListEntitlements)},
	{Method: GET, Path: "/v1/me/product-access", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: params(Param{Name: "live", Kind: "boolean"}, pageParams), Responses: []Reply{{200, billing.ListPage[billing.ProductAccessGrant]{}}}, Errors: codes("authentication_required", "invalid_cursor", "invalid_param", "invalid_query"), Handler: h(handlers.SelfListProductAccess)},
}
