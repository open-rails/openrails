package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// meteringRoutes is spend as it happens: a customer's usage report and
// series, its own and staff's, the charges that run at a rate until they are
// stopped, and the provider operations a host settles against its own
// upstream costs. Usage itself arrives in lease syncs, which gates decide
// against (limits.go).
var meteringRoutes = []Route{
	{Method: GET, Path: "/v1/me/usage", EventStore: true, Group: Customer, Auth: AuthCustomer,
		Query: billing.GetUsageParams{}, Responses: []Reply{{200, billing.Usage{}}}, Errors: codes("currency_unsupported", "invalid_cursor"), Handler: h(handlers.GetMyUsage)},
	{Method: GET, Path: "/v1/me/usage/series", EventStore: true, Group: Customer, Auth: AuthCustomer,
		Query: billing.GetUsageSeriesParams{}, Responses: []Reply{{200, billing.UsageSeries{}}}, Errors: codes("currency_unsupported", "too_many_buckets"), Handler: h(handlers.GetMyUsageSeries)},
	{Method: GET, Path: "/v1/admin/customers/{customer_id}/usage", EventStore: true, Group: Admin, Auth: AuthMerchant, Name: "GetUsage", Level: LevelRead,
		Args: []any{billing.CustomerID{}},
		Doc: "GetUsage reads a page of a customer's usage report in params.Currency: its groups over the days of " +
			"params.Timezone (the merchant's accounting zone when empty) overlapping [From, To), as the customer reads it.",
		Query: billing.GetUsageParams{}, Responses: []Reply{{200, billing.Usage{}}}, Errors: withCustomer("currency_unsupported", "invalid_cursor"), Handler: h(handlers.GetUsage)},
	{Method: GET, Path: "/v1/admin/customers/{customer_id}/usage/series", EventStore: true, Group: Admin, Auth: AuthMerchant, Name: "GetUsageSeries", Level: LevelRead,
		Args: []any{billing.CustomerID{}},
		Doc: "GetUsageSeries reads a customer's usage over time in params.Currency, as the customer reads it: one " +
			"bucket per params.Interval on params.Timezone's calendar overlapping [From, To). More than " +
			"billing.MaxUsageBuckets buckets is refused (billing.CodeTooManyBuckets).",
		Query: billing.GetUsageSeriesParams{}, Responses: []Reply{{200, billing.UsageSeries{}}}, Errors: withCustomer("currency_unsupported", "too_many_buckets"), Handler: h(handlers.GetUsageSeries)},
	{Method: POST, Path: "/v1/app/running-charges", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "StartRunningCharge",
		Doc: "StartRunningCharge starts charging a customer at a rate per hour, or answers the charge " +
			"params.Reference already names. A changed term is billing.ErrIdempotencyKeyReused; a MinimumAmount " +
			"the customer's capacity does not cover is billing.ErrInsufficientCredits, with nothing written.",
		Request: billing.StartRunningChargeParams{}, Responses: []Reply{{201, billing.RunningCharge{}}, {200, billing.RunningCharge{}}},
		Errors: codes("currency_unsupported", "idempotency_key_reused", "insufficient_credits", "invalid_param"), Handler: h(handlers.StartRunningCharge)},
	{Method: POST, Path: "/v1/app/running-charges/{reference}/stop", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "StopRunningCharge",
		Args: []any{""},
		Doc: "StopRunningCharge stops the charge reference names and posts the rest of it to the ledger: " +
			"ChargedAmount is final. Stopping it again at the same instant answers it; at another it is " +
			"billing.ErrIdempotencyKeyReused.",
		Request: billing.StopRunningChargeParams{}, Responses: []Reply{{200, billing.RunningCharge{}}},
		Errors: codes("idempotency_key_reused", "invalid_param", "running_charge_not_found"), Handler: h(handlers.StopRunningCharge)},
	{Method: GET, Path: "/v1/app/running-charges/{reference}", Group: App, Auth: AuthApplication, Permission: needUsageRead, Name: "GetRunningCharge",
		Args:      []any{""},
		Doc:       "GetRunningCharge reads the charge reference names, with what it has charged as of now.",
		Responses: []Reply{{200, billing.RunningCharge{}}}, Errors: codes("running_charge_not_found"), Handler: h(handlers.GetRunningCharge)},
	{Method: GET, Path: "/v1/app/running-charges", Group: App, Auth: AuthApplication, Permission: needUsageRead, Name: "ListRunningCharges",
		Doc: "ListRunningCharges is one page of the merchant's running charges, newest first: of params.CustomerID " +
			"and in params.State when set. With params.Depleted it lists only the running charges of customers " +
			"whose capacity has run out, whether or not the watcher has looked yet: confirm with GetBalance, then " +
			"stop what they pay for.",
		Query: billing.RunningChargeListParams{}, Responses: []Reply{{200, billing.ListPage[billing.RunningCharge]{}}},
		Errors: codes("invalid_cursor", "invalid_query"), Handler: h(handlers.ListRunningCharges)},
}
