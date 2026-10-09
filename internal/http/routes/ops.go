package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// opsRoutes is how a merchant watches its books: findings, its inbox,
// worker health, metrics, the dashboard and host events.
var opsRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/host-events", Group: Merchant, Auth: AuthMerchant, Name: "ListHostEvents", Level: LevelRead, Resources: res(ResHostEvents),
		Query: params(queryOf(handlers.HostEventsQuery{}), pageParams), Responses: []Reply{{200, billing.ListPage[billing.HostEvent]{}}}, Errors: codes("invalid_cursor", "invalid_host_event_request"), Handler: h(handlers.ServiceListHostEvents)},
	{Method: POST, Path: "/v1/merchant/host-events/acknowledge", Group: Merchant, Auth: AuthMerchant, Name: "AcknowledgeHostEvents", Level: LevelWrite, Resources: res(ResHostEvents),
		Request: billing.AcknowledgeHostEventsParams{}, Responses: []Reply{{200, billing.HostEventLookup{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ServiceAcknowledgeHostEvents)},
	{Method: POST, Path: "/v1/merchant/metrics/query", Group: Merchant, Auth: AuthMerchant, Name: "QueryMetrics", Level: LevelRead, Resources: res(ResMetrics),
		Request: billing.MetricsQuery{}, Responses: []Reply{{200, billing.MetricsResult{}}}, Errors: codes("metrics_query_invalid", "service_unavailable"), Handler: h(handlers.MerchantMetricsQuery)},
	{Method: GET, Path: "/v1/merchant/metrics/schema", Group: Merchant, Auth: AuthMerchant, Name: "GetMetricsSchema", Level: LevelRead, Resources: res(ResMetrics),
		Responses: []Reply{{200, billing.MetricsSchema{}}}, Handler: h(handlers.MerchantMetricsSchema)},
	{Method: GET, Path: "/v1/merchant/dashboard", Group: Merchant, Auth: AuthMerchant, Name: "GetDashboard", Level: LevelRead, Resources: res(ResMetrics),
		Responses: []Reply{{200, billing.Dashboard{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.GetMerchantDashboard)},
	{Method: PUT, Path: "/v1/merchant/dashboard", Group: MerchantConfig, Auth: AuthMerchant, Name: "SetDashboard", Level: LevelAdmin, Resources: res(ResDashboard),
		Request: billing.SetDashboardParams{}, Responses: []Reply{{200, billing.Dashboard{}}}, Errors: codes("dashboard_invalid", "service_unavailable"), Handler: h(handlers.PutMerchantDashboard)},
	{Method: GET, Path: "/v1/merchant/notifications", Group: Merchant, Auth: AuthMerchant, Name: "ListMerchantNotifications", Level: LevelRead, Resources: res(ResOperations),
		Query: params(queryOf(handlers.ListMerchantNotificationsQuery{}), pageParams), Responses: []Reply{{200, billing.ListPage[billing.MerchantNotification]{}}}, Errors: codes("invalid_cursor", "service_unavailable"), Handler: h(handlers.ListMerchantNotifications)},
	{Method: GET, Path: "/v1/merchant/notifications/unread-count", Group: Merchant, Auth: AuthMerchant, Name: "GetUnreadNotificationCount", Level: LevelRead, Resources: res(ResOperations),
		Responses: []Reply{{200, billing.UnreadCount{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.MerchantNotificationsUnreadCount)},
	// Reading notifications needs only the permission that lists them.
	{Method: POST, Path: "/v1/merchant/notifications/read", Group: Merchant, Auth: AuthMerchant, Name: "MarkNotificationsRead", Level: LevelRead, Resources: res(ResOperations),
		Request: billing.MarkNotificationsReadParams{}, Responses: []Reply{{200, billing.NotificationLookup{}}}, Errors: codes("invalid_param", "service_unavailable"), Handler: h(handlers.MarkMerchantNotificationsRead)},
	{Method: GET, Path: "/v1/merchant/worker-health", Group: Merchant, Auth: AuthMerchant, Name: "ListWorkerHealth", Level: LevelRead, Resources: res(ResOperations),
		Responses: []Reply{{200, billing.ListPage[billing.WorkerHealth]{}}}, Handler: h(handlers.GetAdminWorkerHealth)},
	{Method: GET, Path: "/v1/merchant/findings", Group: Merchant, Auth: AuthMerchant, Name: "ListFindings", Level: LevelRead, Resources: res(ResFindings),
		Query: params(queryOf(handlers.FindingsQuery{}), pageParams), Responses: []Reply{{200, billing.ListPage[billing.Finding]{}}}, Errors: codes("invalid_cursor", "service_unavailable"), Handler: h(handlers.AdminListFindings)},
	{Method: GET, Path: "/v1/merchant/findings/summary", Group: Merchant, Auth: AuthMerchant, Name: "GetFindingSummary", Level: LevelRead, Resources: res(ResFindings),
		Responses: []Reply{{200, billing.FindingSummary{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.GetFindingSummary)},
	{Method: GET, Path: "/v1/merchant/findings/{id}", Group: Merchant, Auth: AuthMerchant, Name: "GetFinding", Level: LevelRead, Resources: res(ResFindings),
		Responses: []Reply{{200, billing.Finding{}}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.AdminGetFinding)},
	{Method: POST, Path: "/v1/merchant/findings/{id}/resolve", Group: Merchant, Auth: AuthMerchant, Name: "ResolveFinding", Level: LevelWrite, Resources: res(ResFindings), Sensitive: true,
		Request: billing.ResolveFindingParams{}, Responses: []Reply{{200, billing.FindingResolution{}}}, Errors: codes("finding_action_failed", "finding_not_actionable", "idempotency_key_reused", "invalid_param", "payment_not_found", "payment_not_refundable", "provider_cancel_held", "rebill_terms_committed", "refund_failed", "refund_rail_unavailable", "refund_unsupported", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.AdminResolveFinding)},
	{Method: GET, Path: "/v1/me/notifications", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: params(queryOf(handlers.MyNotificationsQuery{}), pageParams), Responses: []Reply{{200, billing.ListPage[billing.Notification]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.GetNotifications)},
	{Method: GET, Path: "/v1/me/notifications/unread-count", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{200, billing.UnreadCount{}}}, Handler: h(handlers.GetUnreadNotificationCount)},
	{Method: POST, Path: "/v1/me/notifications/{id}/read", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{200, billing.Notification{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.MarkNotificationRead)},

	// #756 metrics Q&A and #741 widget generation send aggregate results to
	// the LLM provider: mounted only with llm.api_key (and, for ask, the
	// llm.ask_enabled consent).
	{Method: POST, Path: "/v1/merchant/metrics/ask", Group: Merchant, Auth: AuthMerchant, Name: "AskMetrics", Level: LevelRead, Resources: res(ResMetrics), When: FeatureMetricsAsk,
		Request: billing.AskMetricsParams{}, Responses: []Reply{{200, billing.MetricsAnswer{}}}, Errors: codes("invalid_param", "model_unavailable", "rate_limit_exceeded", "service_unavailable"), Handler: h(handlers.MerchantMetricsAsk)},
	{Method: POST, Path: "/v1/merchant/dashboard/widgets/generate", Group: MerchantConfig, Auth: AuthMerchant, Name: "GenerateDashboardWidget", Level: LevelAdmin, Resources: res(ResDashboard), When: FeatureDashboardGeneration,
		Request: billing.GenerateDashboardWidgetParams{}, Responses: []Reply{{200, billing.GeneratedWidget{}}}, Errors: codes("rate_limit_exceeded", "dashboard_invalid", "invalid_param", "model_unavailable", "service_unavailable", "widget_generation_invalid"), Handler: h(handlers.GenerateDashboardWidget)},
}
