package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

// entitlementsRoutes is access: the entitlements and product access a
// customer holds, and the grants staff make by hand.
var entitlementsRoutes = []Route{
	{Method: POST, Path: "/v1/merchant/customers/entitlements:batch", Group: MerchantAPI, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Request: handlers.ServiceExternalSubjectEntitlementsRequest{}, Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "invalid_param"), Handler: h(handlers.ServiceGetExternalSubjectEntitlements)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/entitlements", Group: MerchantAPI, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("at")), Responses: []Reply{{200, []handlers.ServiceEntitlementRecord{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ServiceGetCustomerEntitlements)},
	{Method: GET, Path: "/v1/merchant/entitlements/{entitlement}/customers", Group: MerchantAPI, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("at"), text("cursor"), integer("limit")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ServiceGetCustomersWithEntitlement)},
	{Method: POST, Path: "/v1/merchant/users/{user_id}/entitlements/check", Group: MerchantAPI, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Request: Untyped{}, Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "invalid_param"), Handler: h(handlers.ServiceCheckEntitlements)},
	{Method: POST, Path: "/v1/merchant/users/{user_id}/product-access/check", Group: MerchantAPI, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Request: Untyped{}, Responses: []Reply{{200, Untyped{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ServiceCheckUserProductAccess)},
	{Method: GET, Path: "/v1/merchant/users/{user_id}/product-access", Group: MerchantAPI, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("cursor"), integer("limit"), text("product_id"), text("product_key")), Responses: []Reply{{200, billing.ProductAccessCheck{}}, {200, billing.ProductAccessList{}}}, Errors: codes("catalog_scope_mismatch", "invalid_param"), Handler: h(handlers.ServiceGetUserProductAccess)},
	{Method: POST, Path: "/v1/merchant/customers/{customer_id}/entitlements", Group: MerchantAdmin, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate, Limit: middleware.AdminOperationGrant,
		Request: billing.GrantEntitlementRequest{}, Responses: []Reply{{201, billing.EntitlementRecord{}}}, Errors: codes("invalid_param"), Bind: gated(handlers.GrantAdminEntitlement)},
	{Method: DELETE, Path: "/v1/merchant/customers/{customer_id}/entitlements/{id}", Group: MerchantAdmin, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate, Limit: middleware.AdminOperationDestructive,
		Responses: []Reply{{200, Message{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.RevokeAdminEntitlement)},
	{Method: POST, Path: "/v1/merchant/customers/{customer_id}/product-access", Group: MerchantAdmin, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate, Limit: middleware.AdminOperationGrant,
		Request: handlers.GrantProductAccessRequest{}, Responses: []Reply{{201, handlers.ProductAccessGrantResponse{}}}, Errors: codes("authentication_required", "catalog_scope_mismatch", "invalid_param"), Bind: gated(handlers.GrantAdminProductAccess)},
	{Method: DELETE, Path: "/v1/merchant/customers/{customer_id}/product-access/{id}", Group: MerchantAdmin, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate, Limit: middleware.AdminOperationDestructive,
		Responses: []Reply{{200, Message{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.RevokeAdminProductAccess)},
	{Method: GET, Path: "/v1/me/entitlements/active", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: params(text("at")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "invalid_param"), Handler: h(handlers.SelfGetActiveEntitlements)},
	{Method: GET, Path: "/v1/me/products", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: params(text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ProductAccessList{}}}, Errors: codes("authentication_required", "catalog_scope_mismatch", "invalid_param"), Handler: h(handlers.GetMyProducts)},
	{Method: GET, Path: "/v1/me/products/{product_id}/access", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{200, billing.ProductAccessCheck{}}}, Errors: codes("authentication_required", "invalid_param"), Handler: h(handlers.GetMyProductAccess)},
}
