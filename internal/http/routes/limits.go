package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

// limitErrors are the codes of a limit declaration.
var limitErrors = []billing.ErrorCode{billing.CodeLimitNotFound, "catalog_revision_conflict", "invalid_param", "resource_conflict", "revision_mismatch"}

// limitsRoutes is usage limits: the catalog's declarations, where a customer
// stands on each in its current window, a gate's lease sync, and staff
// adjusting a window. A limit's capacity comes from the customer's
// products, or an override set with UpdateCustomer.
var limitsRoutes = []Route{
	{Method: GET, Path: "/v1/catalog/limits", Group: CatalogAdmin, Auth: AuthMerchant, Name: "ListLimits", Level: LevelRead,
		Cache: config.ReadMeters,
		Doc:   "ListLimits returns one page of the merchant's limits, by key.",
		Query: billing.PageRequest{}, Responses: []Reply{{200, billing.ListPage[billing.Limit]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListLimits)},
	{Method: GET, Path: "/v1/catalog/limits/{key}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "GetLimit", Level: LevelRead,
		Args:      []any{""},
		Cache:     config.ReadMeters,
		Doc:       "GetLimit reads one limit with the products that give it capacity.",
		Responses: []Reply{{200, billing.Limit{}}}, Errors: codes(billing.CodeLimitNotFound), Handler: h(handlers.GetLimit)},
	{Method: PUT, Path: "/v1/catalog/limits/{key}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "SetLimit", Level: LevelUpdate,
		Args: []any{""},
		Doc: "SetLimit declares the limit under key whole, creating it or changing it. A limit with windows keeps " +
			"its unit (billing.ErrConflict).",
		Request: billing.SetLimitParams{}, Responses: []Reply{{200, billing.Limit{}}}, Errors: codes(limitErrors...), Handler: h(handlers.SetLimit)},

	{Method: GET, Path: "/v1/app/limits", Group: App, Auth: AuthApplication, Permission: needUsageRead, Name: "ListCustomerLimits",
		Doc: "ListCustomerLimits is where params.CustomerID stands on every limit in its current window, in one " +
			"page.",
		Query: billing.CustomerLimitListParams{}, Responses: []Reply{{200, billing.ListPage[billing.CustomerLimit]{}}}, Errors: withCustomer(), Handler: h(handlers.ListCustomerLimits)},
	{Method: GET, Path: "/v1/me/limits", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.ListPage[billing.CustomerLimit]{}}}, Handler: h(handlers.ListMyLimits)},
	{Method: POST, Path: "/v1/app/leases/sync", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "SyncLeases",
		Doc: "SyncLeases syncs one gate's lease: it settles the lease's usage in order, answers each want with a " +
			"hold or a refusal, and takes back the holds returned. The first sync under a new params.LeaseID " +
			"creates the lease.",
		Request: billing.SyncLeasesParams{}, Responses: []Reply{{200, billing.SyncLeasesResult{}}},
		Errors: codes("currency_unsupported", "idempotency_key_reused", "invalid_param", billing.CodeLeaseNotFound, billing.CodeLeaseNotHeld, billing.CodeServiceUnavailable, billing.CodeUsageNotPriced, billing.CodeUsageOutOfOrder), Handler: h(handlers.SyncLeases)},
	{Method: POST, Path: "/v1/admin/customers/{customer_id}/limit-adjustments", Group: Admin, Auth: AuthMerchant, Name: "AdjustCustomerLimit", Level: LevelUpdate, Limit: middleware.AdminOperationGrant,
		Args: []any{billing.CustomerID{}},
		Doc: "AdjustCustomerLimit adds params.Amount to the capacity of the customer's current window of " +
			"params.Limit only, and answers where it stands after. A retry with the same params.SourceID answers " +
			"the line without adding again; other terms under it are billing.ErrIdempotencyKeyReused.",
		Request: billing.AdjustLimitParams{}, Responses: []Reply{{200, billing.CustomerLimit{}}}, Errors: withCustomer("idempotency_key_reused", "invalid_param", billing.CodeLimitNotFound), Handler: h(handlers.AdjustCustomerLimit)},
}
