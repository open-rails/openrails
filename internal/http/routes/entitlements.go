package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

// entitlementsRoutes is access: the products a customer holds, the keys those
// products grant them, and the products staff grant free.
var entitlementsRoutes = []Route{
	{Method: GET, Path: "/v1/admin/customers/{customer_id}/access-tenure", Group: Admin, Auth: AuthMerchant, Name: "GetAccessTenure", Level: LevelRead,
		Args: []any{billing.CustomerID{}},
		Doc: "GetAccessTenure returns the customer's elapsed access to the requested entitlement keys, including " +
			"past product access and retired keys. params.At must not be in the future; zero uses OpenRails' " +
			"clock.",
		Query: billing.AccessTenureParams{}, Responses: []Reply{{200, billing.AccessTenure{}}}, Errors: codes("invalid_param", "service_credential_customer_scope_denied"), Handler: h(handlers.GetAccessTenure)},
	{Method: GET, Path: "/v1/me/access-tenure", Group: Customer, Auth: AuthCustomer,
		Query: billing.AccessTenureParams{}, Responses: []Reply{{200, billing.AccessTenure{}}}, Errors: codes("authentication_required", "invalid_param"), Handler: h(handlers.SelfAccessTenure)},
	{Method: GET, Path: "/v1/admin/entitlements", Group: Admin, Auth: AuthMerchant, Name: "ListEntitlements", Level: LevelRead,
		Doc: "ListEntitlements returns one page of the keys customers hold at params.At (zero: now), by customer " +
			"then key in byte order: of params.CustomerIDs (1 to billing.MaxBatchItems), only params.Entitlements " +
			"and keys under params.Prefix. A key absent from the answer is not held. Without customers, " +
			"params.Entitlements names one key and the page is its holders.",
		Query: billing.EntitlementListParams{}, Responses: []Reply{{200, billing.ListPage[billing.CustomerEntitlement]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "service_credential_customer_scope_denied"), Handler: h(handlers.ListEntitlements)},
	{Method: POST, Path: "/v1/app/entitlements/check", Group: App, Auth: AuthApplication, Permission: needEntitlements, Name: "CheckEntitlements", Level: LevelRead,
		Client: HandWritten,
		Doc: "CheckEntitlements is the programmatic content gate: which of params.Entitlements params.CustomerID " +
			"holds, and the keys they hold under each of params.Prefixes, at params.At (zero: now); every " +
			"requested key and prefix is in the answer. One call checks at most billing.MaxEntitlementChecks keys " +
			"and billing.MaxEntitlementPrefixes prefixes; more is refused with invalid_param, not split. It needs " +
			"an application holding billing.MerchantEntitlementsRead or RootEntitlementsRead " +
			"(RouteGroups.Programmatic). A remote client answers a current exact-key check from its entitlement " +
			"cache when every key is allowed there (EntitlementCheckCacheOptions), unless the handle is Uncached; " +
			"a denial always asks OpenRails.",
		Request: billing.CheckEntitlementsParams{}, Responses: []Reply{{200, billing.EntitlementCheck{}}}, Errors: codes("invalid_param", "service_credential_customer_scope_denied"), Handler: h(handlers.AppCheckEntitlements)},
	{Method: GET, Path: "/v1/admin/product-access", Group: Admin, Auth: AuthMerchant, Name: "ListProductAccess", Level: LevelRead,
		Doc: "ListProductAccess returns one page of product-access windows, newest first: bought, subscribed and " +
			"granted, of params.CustomerIDs and params.ProductIDs (each 1 to billing.MaxBatchItems; none: any). " +
			"params.LiveOnly keeps those live now. params.IDs instead reads named windows.",
		Query: billing.ProductAccessListParams{}, Responses: []Reply{{200, billing.ListPage[billing.ProductAccessGrant]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "service_credential_customer_scope_denied"), Handler: h(handlers.ListProductAccess)},
	{Method: POST, Path: "/v1/admin/product-access", Group: Admin, Auth: AuthMerchant, Name: "CreateProductAccess", Level: LevelUpdate, Limit: middleware.AdminOperationGrant, IdempotencyKey: true,
		Doc: "CreateProductAccess grants 1 to billing.MaxBatchItems products free, across any customers, all or " +
			"none; the answer is in request order. Each grant is a window of its product: the customer holds the " +
			"product's current keys while it is live. A retry with idempotencyKey replays the first answer; with " +
			"none, OpenRails names the batch itself.",
		Request: billing.CreateProductAccessBatchParams{}, Responses: []Reply{{201, billing.CreateProductAccessBatchResult{}}}, Errors: codes("authentication_required", "idempotency_key_reused", "invalid_param", "resource_not_found", "service_credential_customer_scope_denied"), Handler: h(handlers.CreateProductAccess)},
	{Method: POST, Path: "/v1/admin/product-access/{id}/revoke", Group: Admin, Auth: AuthMerchant, Name: "RevokeProductAccess", Level: LevelUpdate,
		Args: []any{billing.ProductAccessID{}},
		Doc: "RevokeProductAccess takes back a product a customer holds, with params.Reason; a window not yet " +
			"started is removed. Revoking again changes nothing.",
		Request: billing.RevokeProductAccessParams{}, Responses: []Reply{{204, nil}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.RevokeProductAccess)},
	{Method: GET, Path: "/v1/me/entitlements", Group: Customer, Auth: AuthCustomer,
		Query: handlers.MyEntitlementListQuery{}, Responses: []Reply{{200, billing.ListPage[billing.CustomerEntitlement]{}}}, Errors: codes("authentication_required", "invalid_cursor", "invalid_param", "invalid_query"), Handler: h(handlers.SelfListEntitlements)},
}
