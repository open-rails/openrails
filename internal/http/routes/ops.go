package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// opsRoutes is how a merchant watches its books: findings (the one inbox),
// worker health, metrics, the dashboard and host events.
var opsRoutes = []Route{
	{Method: GET, Path: "/v1/app/events", Group: App, Auth: AuthApplication, Permission: needEvents, Name: "ListEvents", Query: queryOf(handlers.EventLogQuery{}), Responses: []Reply{{200, billing.EventPage{}}}, Errors: codes("invalid_cursor", "invalid_query", "invalid_param", "event_cursor_expired"), Handler: h(handlers.ListEvents)},
	{Method: GET, Path: "/v1/merchant/destructive-holds", Group: MerchantConfig, Auth: AuthMerchant, Name: "ListDestructiveHolds", Level: LevelRead, Query: params(page, idsParam), Responses: []Reply{{200, billing.ListPage[billing.DestructiveHold]{}}}, Errors: codes("invalid_cursor", "service_unavailable"), Handler: h(handlers.ListDestructiveHolds)},
	{Method: POST, Path: "/v1/merchant/destructive-holds/{id}/clear", Group: MerchantConfig, Auth: AuthMerchant, Name: "ClearDestructiveHold", Level: LevelUpdate, Request: billing.ClearDestructiveHoldParams{}, Responses: []Reply{{200, billing.DestructiveHold{}}}, Errors: codes("invalid_param", "permission_required", "resource_not_found", "resource_conflict", "service_unavailable"), Bind: incidentClear},

	{Method: GET, Path: "/v1/admin/access", Group: Access, Auth: AuthSignedIn, Name: "GetAdminAccess",
		Responses: []Reply{{200, billing.AdminAccess{}}}, Bind: adminAccess},
	{Method: POST, Path: "/v1/metrics/query", Group: Metrics, Auth: AuthMerchant, Name: "QueryMetrics",
		Request: billing.MetricsQuery{}, Responses: []Reply{{200, billing.MetricsResult{}}}, Errors: codes("metrics_query_invalid", "service_unavailable"), Handler: h(handlers.MerchantMetricsQuery)},
	{Method: GET, Path: "/v1/metrics/schema", Group: Metrics, Auth: AuthMerchant, Name: "GetMetricsSchema",
		Responses: []Reply{{200, billing.MetricsSchema{}}}, Handler: h(handlers.MerchantMetricsSchema)},
	{Method: GET, Path: "/v1/metrics/dashboard", Group: Metrics, Auth: AuthMerchant, Name: "GetDashboard",
		Responses: []Reply{{200, billing.Dashboard{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.GetMerchantDashboard)},
	{Method: PUT, Path: "/v1/merchant/dashboard", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "SetDashboard",
		Request: billing.SetDashboardParams{}, Responses: []Reply{{200, billing.Dashboard{}}}, Errors: codes("dashboard_invalid", "service_unavailable"), Handler: h(handlers.PutMerchantDashboard)},
	{Method: GET, Path: "/v1/merchant/findings", Group: MerchantConfig, Auth: AuthMerchant, Name: "ListFindings", Level: LevelRead,
		Query: params(queryOf(handlers.FindingsQuery{}), idsParam, pageParams), Responses: []Reply{{200, billing.ListPage[billing.Finding]{}}}, Errors: codes("invalid_cursor", "service_unavailable"), Handler: h(handlers.AdminListFindings)},
	{Method: GET, Path: "/v1/merchant/findings/{id}", Group: MerchantConfig, Auth: AuthMerchant, Name: "GetFinding", Level: LevelRead,
		Responses: []Reply{{200, billing.Finding{}}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.AdminGetFinding)},
	{Method: POST, Path: "/v1/merchant/findings/{id}/resolve", Group: MerchantConfig, Auth: AuthMerchant, Name: "ResolveFinding", Level: LevelUpdate,
		Request: billing.ResolveFindingParams{}, Responses: []Reply{{200, billing.FindingResolution{}}}, Errors: codes("finding_action_failed", "finding_not_actionable", "finding_requires_merchant_config", "idempotency_key_reused", "invalid_param", "payment_not_found", "payment_not_refundable", "provider_cancel_held", "rebill_terms_committed", "refund_failed", "refund_rail_unavailable", "refund_unsupported", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.AdminResolveFinding)},
}
