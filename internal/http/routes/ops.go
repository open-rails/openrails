package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// opsRoutes is how a merchant watches its books: findings, repair alerts,
// worker health, notifications, metrics, the dashboard and host events.
var opsRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/host-events", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantHostEventsRead,
		Query: params(queryOf(handlers.HostEventsQuery{}), pageParams), Responses: []Reply{{200, billing.ListPage[billing.HostEvent]{}}}, Errors: codes("invalid_cursor", "invalid_host_event_request"), Handler: h(handlers.ServiceListHostEvents)},
	{Method: POST, Path: "/v1/merchant/host-events/{id}/acknowledge", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantHostEventsAcknowledge,
		Responses: []Reply{{200, billing.HostEvent{}}}, Errors: codes("host_event_not_found", "invalid_param"), Handler: h(handlers.ServiceAcknowledgeHostEvent)},
	{Method: POST, Path: "/v1/merchant/metrics/query", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantMetricsRead,
		Request: billing.MetricsQuery{}, Responses: []Reply{{200, billing.MetricsResult{}}}, Errors: codes("metrics_query_invalid", "service_unavailable"), Handler: h(handlers.MerchantMetricsQuery)},
	{Method: GET, Path: "/v1/merchant/metrics/schema", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantMetricsRead,
		Responses: []Reply{{200, billing.MetricsSchema{}}}, Handler: h(handlers.MerchantMetricsSchema)},
	{Method: GET, Path: "/v1/merchant/dashboard", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantMetricsRead,
		Responses: []Reply{{200, billing.Dashboard{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.GetMerchantDashboard)},
	{Method: PUT, Path: "/v1/merchant/dashboard", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantDashboardUpdate,
		Request: billing.SetDashboardParams{}, Responses: []Reply{{200, billing.Dashboard{}}}, Errors: codes("dashboard_invalid", "service_unavailable"), Handler: h(handlers.PutMerchantDashboard)},
	{Method: GET, Path: "/v1/merchant/notifications", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantMetricsRead,
		Query: params(queryOf(handlers.ListMerchantNotificationsQuery{}), pageParams), Responses: []Reply{{200, billing.ListPage[billing.MerchantNotification]{}}}, Errors: codes("invalid_cursor", "service_unavailable"), Handler: h(handlers.ListMerchantNotifications)},
	{Method: GET, Path: "/v1/merchant/notifications/unread-count", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantMetricsRead,
		Responses: []Reply{{200, billing.UnreadCount{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.MerchantNotificationsUnreadCount)},
	// Reading a notification needs only the permission that lists it.
	{Method: POST, Path: "/v1/merchant/notifications/{id}/read", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantMetricsRead,
		Responses: []Reply{{200, billing.MerchantNotification{}}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.MarkMerchantNotificationRead)},
	{Method: GET, Path: "/v1/merchant/repair-alerts", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantRepairAlertsRead,
		Query: params(queryOf(handlers.RepairAlertsQuery{}), pageParams), Responses: []Reply{{200, billing.ListPage[billing.Notification]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.GetAdminRepairAlerts)},
	{Method: GET, Path: "/v1/merchant/worker-health", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantRepairAlertsRead,
		Responses: []Reply{{200, billing.ListPage[billing.WorkerHealth]{}}}, Handler: h(handlers.GetAdminWorkerHealth)},
	{Method: GET, Path: "/v1/merchant/findings", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantRepairAlertsRead,
		Query: params(queryOf(handlers.FindingsQuery{}), pageParams), Responses: []Reply{{200, billing.ListPage[billing.Finding]{}}}, Errors: codes("invalid_cursor", "service_unavailable"), Handler: h(handlers.AdminListFindings)},
	{Method: GET, Path: "/v1/merchant/findings/summary", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantRepairAlertsRead,
		Responses: []Reply{{200, billing.FindingSummary{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.GetFindingSummary)},
	{Method: GET, Path: "/v1/merchant/findings/{id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantRepairAlertsRead,
		Responses: []Reply{{200, billing.Finding{}}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.AdminGetFinding)},
	{Method: POST, Path: "/v1/merchant/findings/{id}/resolve", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantFindingsResolve,
		Request: billing.ResolveFindingParams{}, Responses: []Reply{{200, billing.FindingResolution{}}}, Errors: codes("invalid_param", "provider_cancel_held", "rebill_terms_committed", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.AdminResolveFinding)},
	{Method: GET, Path: "/v1/me/notifications", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: params(queryOf(handlers.MyNotificationsQuery{}), pageParams), Responses: []Reply{{200, billing.ListPage[billing.Notification]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.GetNotifications)},
	{Method: GET, Path: "/v1/me/notifications/unread-count", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{200, billing.UnreadCount{}}}, Handler: h(handlers.GetUnreadNotificationCount)},
	{Method: POST, Path: "/v1/me/notifications/{id}/read", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{200, billing.Notification{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.MarkNotificationRead)},

	// #756 metrics Q&A and #741 widget generation send aggregate results to
	// the LLM provider: mounted only with llm.api_key (and, for ask, the
	// llm.ask_enabled consent).
	{Method: POST, Path: "/v1/merchant/metrics/ask", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantMetricsRead, When: FeatureMetricsAsk,
		Request: billing.AskMetricsParams{}, Responses: []Reply{{200, billing.MetricsAnswer{}}}, Errors: codes("invalid_param", "model_unavailable", "rate_limit_exceeded", "service_unavailable"), Handler: h(handlers.MerchantMetricsAsk)},
	{Method: POST, Path: "/v1/merchant/dashboard/widgets/generate", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantDashboardUpdate, When: FeatureDashboardGeneration,
		Request: billing.GenerateDashboardWidgetParams{}, Responses: []Reply{{200, billing.GeneratedWidget{}}}, Errors: codes("dashboard_invalid", "invalid_param", "model_unavailable", "service_unavailable", "widget_generation_invalid"), Handler: h(handlers.GenerateDashboardWidget)},
}
