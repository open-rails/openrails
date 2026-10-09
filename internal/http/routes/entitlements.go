package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

// entitlementsRoutes is access: the products a customer holds, the keys those
// products grant them, and the products staff grant free.
var entitlementsRoutes = []Route{
	{Method: GET, Path: "/v1/admin/customers/{customer_id}/entitlements", Group: Admin, Auth: AuthMerchant, Name: "ListCustomerEntitlements", Level: LevelRead,
		Query: params(text("at"), text("prefix"), pageParams), Responses: []Reply{{200, billing.ListPage[billing.CustomerEntitlement]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "service_credential_customer_scope_denied"), Handler: h(handlers.ServiceListCustomerEntitlements)},
	{Method: GET, Path: "/v1/admin/entitlements/{entitlement}/customers", Group: Admin, Auth: AuthMerchant, Name: "ListEntitlementCustomers", Level: LevelRead,
		Query: params(text("at"), pageParams), Responses: []Reply{{200, billing.ListPage[billing.CustomerID]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query"), Handler: h(handlers.ServiceListEntitlementCustomers)},
	{Method: POST, Path: "/v1/admin/customers/{customer_id}/entitlements/check", Group: Admin, Auth: AuthMerchant, Name: "CheckEntitlements", Level: LevelRead,
		Request: billing.CheckEntitlementsParams{}, Responses: []Reply{{200, billing.EntitlementCheck{}}}, Errors: codes("invalid_param", "service_credential_customer_scope_denied"), Handler: h(handlers.ServiceCheckEntitlements)},
	{Method: POST, Path: "/v1/admin/tiers/lookup", Group: Admin, Auth: AuthMerchant, Name: "GetEffectiveTiers", Level: LevelRead,
		Request: billing.GetEffectiveTiersParams{}, Responses: []Reply{{200, billing.EffectiveTierLookup{}}}, Errors: codes("invalid_param", "service_credential_customer_scope_denied"), Handler: h(handlers.GetEffectiveTiers)},
	{Method: POST, Path: "/v1/admin/customers/{customer_id}/product-access/check", Group: Admin, Auth: AuthMerchant, Name: "CheckProductAccess", Level: LevelRead,
		Request: billing.CheckProductAccessParams{}, Responses: []Reply{{200, billing.ProductAccessCheck{}}}, Errors: codes("invalid_param", "service_credential_customer_scope_denied"), Handler: h(handlers.CheckProductAccess)},
	{Method: GET, Path: "/v1/admin/customers/{customer_id}/product-access", Group: Admin, Auth: AuthMerchant, Name: "ListProductAccess", Level: LevelRead,
		Query: params(Param{Name: "live", Kind: "boolean"}, idsParam, pageParams), Responses: []Reply{{200, billing.ListPage[billing.ProductAccessGrant]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "service_credential_customer_scope_denied"), Handler: h(handlers.ListProductAccess)},
	{Method: POST, Path: "/v1/admin/product-access", Group: Admin, Auth: AuthMerchant, Name: "CreateProductAccess", Level: LevelWrite, Sensitive: true, Limit: middleware.AdminOperationGrant, IdempotencyKey: true,
		Request: billing.CreateProductAccessBatchParams{}, Responses: []Reply{{201, billing.CreateProductAccessBatchResult{}}}, Errors: codes("authentication_required", "idempotency_key_reused", "invalid_param", "resource_not_found", "service_credential_customer_scope_denied"), Handler: h(handlers.CreateProductAccess)},
	{Method: DELETE, Path: "/v1/admin/customers/{customer_id}/product-access/{id}", Group: Admin, Auth: AuthMerchant, Name: "DeleteProductAccess", Level: LevelWrite, Sensitive: true, Limit: middleware.AdminOperationDestructive,
		Responses: []Reply{{204, nil}}, Errors: codes("invalid_param", "resource_not_found", "service_credential_customer_scope_denied"), Handler: h(handlers.DeleteProductAccess)},
	{Method: GET, Path: "/v1/me/entitlements", Group: Customer, Auth: AuthCustomer,
		Query: params(text("at"), text("prefix"), pageParams), Responses: []Reply{{200, billing.ListPage[billing.CustomerEntitlement]{}}}, Errors: codes("authentication_required", "invalid_cursor", "invalid_param", "invalid_query"), Handler: h(handlers.SelfListEntitlements)},
	{Method: GET, Path: "/v1/me/product-access", Group: Customer, Auth: AuthCustomer,
		Query: params(Param{Name: "live", Kind: "boolean"}, pageParams), Responses: []Reply{{200, billing.ListPage[billing.ProductAccessGrant]{}}}, Errors: codes("authentication_required", "invalid_cursor", "invalid_param", "invalid_query"), Handler: h(handlers.SelfListProductAccess)},
}
