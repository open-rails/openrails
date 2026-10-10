package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

// entitlementsRoutes is access: the products a customer holds, the keys those
// products grant them, and the products staff grant free.
var entitlementsRoutes = []Route{
	{Method: GET, Path: "/v1/admin/entitlements", Group: Admin, Auth: AuthMerchant, Name: "ListEntitlements", Level: LevelRead,
		Query: params(text("at"), text("customer_id"), repeated("entitlement"), text("prefix"), pageParams), Responses: []Reply{{200, billing.ListPage[billing.CustomerEntitlement]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "service_credential_customer_scope_denied"), Handler: h(handlers.ListEntitlements)},
	{Method: POST, Path: "/v1/admin/tiers/lookup", Group: Admin, Auth: AuthMerchant, Name: "GetEffectiveTiers", Level: LevelRead,
		Request: billing.GetEffectiveTiersParams{}, Responses: []Reply{{200, billing.EffectiveTierLookup{}}}, Errors: codes("invalid_param", "service_credential_customer_scope_denied"), Handler: h(handlers.GetEffectiveTiers)},
	{Method: GET, Path: "/v1/admin/product-access", Group: Admin, Auth: AuthMerchant, Name: "ListProductAccess", Level: LevelRead,
		Query: params(text("customer_id"), Param{Name: "live", Kind: "boolean"}, idsParam, pageParams, text("product_id")), Responses: []Reply{{200, billing.ListPage[billing.ProductAccessGrant]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "service_credential_customer_scope_denied"), Handler: h(handlers.ListProductAccess)},
	{Method: POST, Path: "/v1/admin/product-access", Group: Admin, Auth: AuthMerchant, Name: "CreateProductAccess", Level: LevelWrite, Sensitive: true, Limit: middleware.AdminOperationGrant, IdempotencyKey: true,
		Request: billing.CreateProductAccessBatchParams{}, Responses: []Reply{{201, billing.CreateProductAccessBatchResult{}}}, Errors: codes("authentication_required", "idempotency_key_reused", "invalid_param", "resource_not_found", "service_credential_customer_scope_denied"), Handler: h(handlers.CreateProductAccess)},
	{Method: DELETE, Path: "/v1/admin/customers/{customer_id}/product-access/{id}", Group: Admin, Auth: AuthMerchant, Name: "DeleteProductAccess", Level: LevelWrite, Sensitive: true, Limit: middleware.AdminOperationDestructive,
		Responses: []Reply{{204, nil}}, Errors: codes("invalid_param", "resource_not_found", "service_credential_customer_scope_denied"), Handler: h(handlers.DeleteProductAccess)},
	{Method: GET, Path: "/v1/me/entitlements", Group: Customer, Auth: AuthCustomer,
		Query: params(text("at"), text("prefix"), pageParams), Responses: []Reply{{200, billing.ListPage[billing.CustomerEntitlement]{}}}, Errors: codes("authentication_required", "invalid_cursor", "invalid_param", "invalid_query"), Handler: h(handlers.SelfListEntitlements)},
	{Method: GET, Path: "/v1/me/product-access", Group: Customer, Auth: AuthCustomer,
		Query: params(Param{Name: "live", Kind: "boolean"}, pageParams), Responses: []Reply{{200, billing.ListPage[billing.ProductAccessGrant]{}}}, Errors: codes("authentication_required", "invalid_cursor", "invalid_param", "invalid_query"), Handler: h(handlers.SelfListProductAccess)},
}
