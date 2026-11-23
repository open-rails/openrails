package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

// limitErrors are the codes of a limit declaration.
var limitErrors = []string{billing.CodeLimitNotFound, "catalog_revision_conflict", "invalid_param", "resource_conflict", "revision_mismatch"}

// limitsRoutes is usage limits: the catalog's declarations, where a customer
// stands on each in its current window, a gate's lease sync, and staff
// adjusting a window. A limit's capacity comes from the customer's
// products, or an override set with UpdateCustomer.
var limitsRoutes = []Route{
	{Method: GET, Path: "/v1/catalog/limits", Group: CatalogAdmin, Auth: AuthMerchant, Name: "ListLimits", Level: LevelRead,
		Query: page, Responses: []Reply{{200, billing.ListPage[billing.Limit]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListLimits)},
	{Method: GET, Path: "/v1/catalog/limits/{key}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "GetLimit", Level: LevelRead,
		Responses: []Reply{{200, billing.Limit{}}}, Errors: codes(billing.CodeLimitNotFound), Handler: h(handlers.GetLimit)},
	{Method: PUT, Path: "/v1/catalog/limits/{key}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "SetLimit", Level: LevelUpdate,
		Request: billing.SetLimitParams{}, Responses: []Reply{{200, billing.Limit{}}}, Errors: codes(limitErrors...), Handler: h(handlers.SetLimit)},

	{Method: GET, Path: "/v1/app/limits", Group: App, Auth: AuthApplication, Permission: needUsageRead, Name: "ListCustomerLimits",
		Query: params(text("customer_id")), Responses: []Reply{{200, billing.ListPage[billing.CustomerLimit]{}}}, Errors: withCustomer(), Handler: h(handlers.ListCustomerLimits)},
	{Method: GET, Path: "/v1/me/limits", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.ListPage[billing.CustomerLimit]{}}}, Handler: h(handlers.ListMyLimits)},
	{Method: POST, Path: "/v1/app/leases/sync", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "SyncLeases",
		Request: billing.SyncLeasesParams{}, Responses: []Reply{{200, billing.SyncLeasesResult{}}},
		Errors: codes("currency_unsupported", "idempotency_key_reused", "invalid_param", billing.CodeLeaseNotFound, billing.CodeLeaseNotHeld, billing.CodeUsageNotPriced, billing.CodeUsageOutOfOrder), Handler: h(handlers.SyncLeases)},
	{Method: POST, Path: "/v1/admin/customers/{customer_id}/limit-adjustments", Group: Admin, Auth: AuthMerchant, Name: "AdjustCustomerLimit", Level: LevelUpdate, Limit: middleware.AdminOperationGrant,
		Request: billing.AdjustLimitParams{}, Responses: []Reply{{200, billing.CustomerLimit{}}}, Errors: withCustomer("idempotency_key_reused", "invalid_param", billing.CodeLimitNotFound), Handler: h(handlers.AdjustCustomerLimit)},
}
