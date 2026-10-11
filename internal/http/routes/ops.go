package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// opsRoutes is how a merchant watches its books: findings (the one inbox),
// worker health, metrics, the dashboard and host events.
var opsRoutes = []Route{
	{Method: GET, Path: "/v1/app/host-events", Group: App, Auth: AuthApplication, Permission: needEvents, Name: "ListHostEvents",
		Query: params(queryOf(handlers.HostEventsQuery{}), idsParam, pageParams), Responses: []Reply{{200, billing.ListPage[billing.HostEvent]{}}}, Errors: codes("invalid_cursor", "invalid_host_event_request"), Handler: h(handlers.ServiceListHostEvents)},
	{Method: POST, Path: "/v1/app/host-events/acknowledge", Group: App, Auth: AuthApplication, Permission: needEvents, Name: "AcknowledgeHostEvents",
		Request: billing.AcknowledgeHostEventsParams{}, Responses: []Reply{{200, billing.HostEventLookup{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ServiceAcknowledgeHostEvents)},
	{Method: GET, Path: "/v1/admin/access", Group: Access, Auth: AuthSignedIn, Name: "GetAdminAccess",
		Responses: []Reply{{200, billing.AdminAccess{}}}, Bind: adminAccess},
	{Method: POST, Path: "/v1/metrics/query", Group: Metrics, Auth: AuthMerchant, Name: "QueryMetrics",
		Request: billing.MetricsQuery{}, Responses: []Reply{{200, billing.MetricsResult{}}}, Errors: codes("metrics_query_invalid", "service_unavailable"), Handler: h(handlers.MerchantMetricsQuery)},
	{Method: GET, Path: "/v1/metrics/schema", Group: Metrics, Auth: AuthMerchant, Name: "GetMetricsSchema",
		Responses: []Reply{{200, billing.MetricsSchema{}}}, Handler: h(handlers.MerchantMetricsSchema)},
	{Method: GET, Path: "/v1/metrics/dashboard", Group: Metrics, Auth: AuthMerchant, Name: "GetDashboard",
		Responses: []Reply{{200, billing.Dashboard{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.GetMerchantDashboard)},
	// Metrics questions send aggregate results to the LLM provider, requiring
	// both llm.api_key and the llm.ask_enabled consent.
	{Method: POST, Path: "/v1/metrics/ask", Group: Metrics, Auth: AuthMerchant, Name: "AskMetrics", When: FeatureMetricsAsk,
		Request: billing.AskMetricsParams{}, Responses: []Reply{{200, billing.MetricsAnswer{}}}, Errors: codes("invalid_param", "model_unavailable", "rate_limit_exceeded", "service_unavailable"), Handler: h(handlers.MerchantMetricsAsk)},
	{Method: PUT, Path: "/v1/merchant/dashboard", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "SetDashboard",
		Request: billing.SetDashboardParams{}, Responses: []Reply{{200, billing.Dashboard{}}}, Errors: codes("dashboard_invalid", "service_unavailable"), Handler: h(handlers.PutMerchantDashboard)},
	{Method: GET, Path: "/v1/merchant/findings", Group: MerchantConfig, Auth: AuthMerchant, Name: "ListFindings", Level: LevelRead,
		Query: params(queryOf(handlers.FindingsQuery{}), idsParam, pageParams), Responses: []Reply{{200, billing.ListPage[billing.Finding]{}}}, Errors: codes("invalid_cursor", "service_unavailable"), Handler: h(handlers.AdminListFindings)},
	{Method: GET, Path: "/v1/merchant/findings/{id}", Group: MerchantConfig, Auth: AuthMerchant, Name: "GetFinding", Level: LevelRead,
		Responses: []Reply{{200, billing.Finding{}}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.AdminGetFinding)},
	{Method: POST, Path: "/v1/merchant/findings/{id}/resolve", Group: MerchantConfig, Auth: AuthMerchant, Name: "ResolveFinding", Level: LevelUpdate,
		Request: billing.ResolveFindingParams{}, Responses: []Reply{{200, billing.FindingResolution{}}}, Errors: codes("finding_action_failed", "finding_not_actionable", "idempotency_key_reused", "invalid_param", "payment_not_found", "payment_not_refundable", "provider_cancel_held", "rebill_terms_committed", "refund_failed", "refund_rail_unavailable", "refund_unsupported", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.AdminResolveFinding)},
	{Method: GET, Path: "/v1/me/notifications", Group: Customer, Auth: AuthCustomer,
		Query: params(queryOf(handlers.MyNotificationsQuery{}), pageParams), Responses: []Reply{{200, billing.ListPage[billing.Notification]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.GetNotifications)},
	{Method: POST, Path: "/v1/me/notifications/read", Group: Customer, Auth: AuthCustomer,
		Request: billing.MarkNotificationsReadParams{}, Responses: []Reply{{200, billing.CustomerNotificationLookup{}}}, Errors: codes("invalid_param"), Handler: h(handlers.MarkMyNotificationsRead)},
}
