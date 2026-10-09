package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

// entitlementsRoutes is access: the entitlements and product access a
// customer holds, and the grants staff make by hand.
var entitlementsRoutes = []Route{
	{Method: POST, Path: "/v1/merchant/entitlements/lookup", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Request: billing.EntitlementListParams{}, Responses: []Reply{{200, billing.EntitlementLookup{}}}, Errors: codes("invalid_param", "service_credential_customer_scope_denied"), Handler: h(handlers.ServiceListEntitlements)},
	{Method: GET, Path: "/v1/merchant/entitlements/{entitlement}/customers", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("at"), text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.CustomerID]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query"), Handler: h(handlers.ServiceListEntitlementCustomers)},
	{Method: POST, Path: "/v1/merchant/customers/{customer_id}/entitlements/check", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Request: billing.CheckEntitlementsParams{}, Responses: []Reply{{200, billing.EntitlementCheck{}}}, Errors: codes("invalid_param", "service_credential_customer_scope_denied"), Handler: h(handlers.ServiceCheckEntitlements)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/tier", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("group")), Responses: []Reply{{200, billing.EffectiveTier{}}}, Errors: codes("invalid_param", "service_credential_customer_scope_denied"), Handler: h(handlers.ServiceGetEffectiveTier)},
	{Method: POST, Path: "/v1/merchant/customers/{customer_id}/product-access/check", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Request: billing.CheckProductAccessParams{}, Responses: []Reply{{200, billing.ProductAccessCheck{}}}, Errors: codes("invalid_param", "service_credential_customer_scope_denied"), Handler: h(handlers.CheckProductAccess)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/product-access", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Query: params(text("cursor"), integer("limit")), Responses: []Reply{{200, billing.ListPage[billing.ProductAccessGrant]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "service_credential_customer_scope_denied"), Handler: h(handlers.ListProductAccess)},
	{Method: POST, Path: "/v1/merchant/customers/{customer_id}/entitlements", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate, Limit: middleware.AdminOperationGrant,
		Request: billing.CreateEntitlementParams{}, Responses: []Reply{{201, billing.EntitlementRecord{}}}, Errors: codes("invalid_param", "permanent_grant_forbidden", "service_credential_customer_scope_denied"), Bind: gated(handlers.CreateEntitlement)},
	{Method: DELETE, Path: "/v1/merchant/customers/{customer_id}/entitlements/{id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate, Limit: middleware.AdminOperationDestructive,
		Responses: []Reply{{204, nil}}, Errors: codes("invalid_param", "resource_not_found", "service_credential_customer_scope_denied"), Handler: h(handlers.DeleteEntitlement)},
	{Method: POST, Path: "/v1/merchant/product-access", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate, Limit: middleware.AdminOperationGrant,
		Request: billing.CreateProductAccessBatchParams{}, Responses: []Reply{{201, billing.CreateProductAccessBatchResult{}}}, Errors: codes("authentication_required", "invalid_param", "permanent_grant_forbidden", "service_credential_customer_scope_denied"), Bind: gated(handlers.CreateProductAccess)},
	{Method: DELETE, Path: "/v1/merchant/customers/{customer_id}/product-access/{id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate, Limit: middleware.AdminOperationDestructive,
		Responses: []Reply{{204, nil}}, Errors: codes("invalid_param", "resource_not_found", "service_credential_customer_scope_denied"), Handler: h(handlers.DeleteProductAccess)},
	{Method: GET, Path: "/v1/me/entitlements", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: params(text("at")), Responses: []Reply{{200, billing.ListPage[billing.EntitlementRecord]{}}}, Errors: codes("authentication_required", "invalid_cursor", "invalid_param", "invalid_query"), Handler: h(handlers.SelfListEntitlements)},
}
