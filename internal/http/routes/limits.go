package routes

import (
	"github.com/open-rails/openrails/billing"
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
		Cache: billing.ReadMeters,
		Doc:   "ListLimits returns one page of the merchant's limits, by key.",
		Query: billing.PageRequest{}, Responses: []Reply{{200, billing.ListPage[billing.Limit]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListLimits)},
	{Method: GET, Path: "/v1/catalog/limits/{key}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "GetLimit", Level: LevelRead,
		Args:      []any{""},
		Cache:     billing.ReadMeters,
		Doc:       "GetLimit reads one limit with the products that give it capacity.",
		Responses: []Reply{{200, billing.Limit{}}}, Errors: codes(billing.CodeLimitNotFound), Handler: h(handlers.GetLimit)},
	{Method: PUT, Path: "/v1/catalog/limits/{key}", EventStore: true, Group: CatalogAdmin, Auth: AuthMerchant, Name: "SetLimit", Level: LevelUpdate,
		Args: []any{""},
		Doc: "SetLimit declares the limit under key whole, creating it or changing it. A limit with windows keeps " +
			"its unit (billing.ErrConflict).",
		Request: billing.SetLimitParams{}, Responses: []Reply{{200, billing.Limit{}}}, Errors: codes(limitErrors...), Handler: h(handlers.SetLimit)},

	{Method: GET, Path: "/v1/app/limits", EventStore: true, Group: App, Auth: AuthApplication, Permission: needUsageRead, Name: "ListCustomerLimits",
		Doc: "ListCustomerLimits is where params.CustomerID stands on every limit in its current window, in one " +
			"page.",
		Query: billing.CustomerLimitListParams{}, Responses: []Reply{{200, billing.ListPage[billing.CustomerLimit]{}}}, Errors: withCustomer(), Handler: h(handlers.ListCustomerLimits)},
	{Method: GET, Path: "/v1/me/limits", EventStore: true, Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.ListPage[billing.CustomerLimit]{}}}, Handler: h(handlers.ListMyLimits)},
	{Method: POST, Path: "/v1/app/leases/sync", EventStore: true, Group: App, Auth: AuthApplication, Permission: needUsage, Name: "SyncLeases",
		Doc: "SyncLeases syncs one lease, the protocol under Gate (hosts call NewGate). A sync carries the " +
			"lease's usage, numbered within the lease, what its gate has had in flight across a sync and the " +
			"customers whose views it asks for; it settles all the usage it carries and answers how far the " +
			"usage settled (a resend is matched by lease and number), each view (where the customer stands on " +
			"its limits, reserving nothing), the limit set of each invoker's attributes, the pricing a gate " +
			"costs requests with, and how many of the merchant's leases are live. The first sync under a new " +
			"LeaseID creates the lease; a lease answers resends for 61 days after its last sync. Settled usage " +
			"posts to the ledger at each month's end in the merchant's accounting zone and before any other " +
			"movement of the customer's balance.",
		Request: billing.SyncLeasesParams{}, Responses: []Reply{{200, billing.SyncLeasesResult{}}},
		Errors: codes("currency_unsupported", "idempotency_key_reused", "invalid_param", "resource_conflict", billing.CodeLeaseNotFound, billing.CodeServiceUnavailable, billing.CodeUsageNotPriced, billing.CodeUsageOutOfOrder), Handler: h(handlers.SyncLeases)},
	{Method: POST, Path: "/v1/admin/customers/{customer_id}/limit-adjustments", EventStore: true, Group: Admin, Auth: AuthMerchant, Name: "AdjustCustomerLimit", Level: LevelUpdate, Limit: middleware.AdminOperationGrant,
		Args: []any{billing.CustomerID{}},
		Doc: "AdjustCustomerLimit adds params.Amount to the capacity of the customer's current window of " +
			"params.Limit only, and answers where it stands after. A retry with the same params.SourceID answers " +
			"the line without adding again; other terms under it are billing.ErrIdempotencyKeyReused.",
		Request: billing.AdjustCustomerLimitParams{}, Responses: []Reply{{200, billing.CustomerLimit{}}}, Errors: withCustomer("idempotency_key_reused", "invalid_param", billing.CodeLimitNotFound), Handler: h(handlers.AdjustCustomerLimit)},
}
