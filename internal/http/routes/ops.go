package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/internal/modules/metrics"
)

// opsRoutes is how a merchant watches its books: findings, repair alerts,
// worker health, notifications, metrics, the dashboard and host events.
var opsRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/host-events", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantHostEventsRead,
		Query: params(text("include_acknowledged"), integer("limit"), text("payment_id"), text("type")), Responses: []Reply{{200, []billing.HostEvent{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ServiceListHostEvents)},
	{Method: POST, Path: "/v1/merchant/host-events/{id}/acknowledge", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantHostEventsAcknowledge,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ServiceAcknowledgeHostEvent)},
	{Method: POST, Path: "/v1/merchant/metrics/query", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantMetricsRead,
		Responses: []Reply{{200, metrics.Result{}}}, Errors: codes("metrics_query_invalid", "service_unavailable"), Handler: h(handlers.MerchantMetricsQuery)},
	{Method: GET, Path: "/v1/merchant/metrics/schema", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantMetricsRead,
		Responses: []Reply{{200, metrics.SchemaDoc{}}}, Handler: h(handlers.MerchantMetricsSchema)},
	{Method: GET, Path: "/v1/merchant/dashboard", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantMetricsRead,
		Responses: []Reply{{200, dashboard.Dashboard{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.GetMerchantDashboard)},
	{Method: PUT, Path: "/v1/merchant/dashboard", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantDashboardUpdate,
		Responses: []Reply{{200, dashboard.Dashboard{}}}, Errors: codes("dashboard_invalid", "service_unavailable"), Handler: h(handlers.PutMerchantDashboard)},
	{Method: GET, Path: "/v1/merchant/notifications", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantMetricsRead,
		Query: params(text("unread")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.ListMerchantNotifications)},
	{Method: GET, Path: "/v1/merchant/notifications/unread-count", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantMetricsRead,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.MerchantNotificationsUnreadCount)},
	{Method: POST, Path: "/v1/merchant/notifications/{id}/read", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSettingsUpdate,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("resource_not_found", "service_unavailable"), Handler: h(handlers.MarkMerchantNotificationRead)},
	{Method: GET, Path: "/v1/merchant/repair-alerts", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantRepairAlertsRead,
		Query: params(integer("limit"), integer("offset"), text("seen")), Responses: []Reply{{200, PathPage[billing.Notification]{}}}, Handler: h(handlers.GetAdminRepairAlerts)},
	{Method: GET, Path: "/v1/merchant/worker-health", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantRepairAlertsRead,
		Responses: []Reply{{200, []handlers.WorkerHealthItem{}}}, Handler: h(handlers.GetAdminWorkerHealth)},
	{Method: GET, Path: "/v1/merchant/findings", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantRepairAlertsRead,
		Query: params(text("finding_type"), integer("limit"), integer("offset"), text("severity"), text("status")), Responses: []Reply{{200, handlers.FindingsListResponse{}}}, Handler: h(handlers.AdminListFindings)},
	{Method: GET, Path: "/v1/merchant/findings/{id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantRepairAlertsRead,
		Responses: []Reply{{200, handlers.FindingView{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.AdminGetFinding)},
	{Method: POST, Path: "/v1/merchant/findings/{id}/resolve", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantFindingsResolve,
		Request: handlers.ResolveFindingRequest{}, Responses: []Reply{{200, handlers.ResolveFindingResponse{}}}, Errors: codes("invalid_param", "provider_cancel_held", "rebill_terms_committed", "resource_conflict", "resource_not_found"), Handler: h(handlers.AdminResolveFinding)},
	{Method: GET, Path: "/v1/me/notifications", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: params(integer("limit"), integer("offset"), text("seen")), Responses: []Reply{{200, PathPage[billing.Notification]{}}}, Handler: h(handlers.GetNotifications)},
	{Method: GET, Path: "/v1/me/notifications/unread-count", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{200, Untyped{}}}, Handler: h(handlers.GetUnreadNotificationCount)},
	{Method: POST, Path: "/v1/me/notifications/{id}/read", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{200, Message{}}}, Errors: codes("invalid_param"), Handler: h(handlers.MarkNotificationRead)},

	// #756 metrics Q&A and #741 widget generation send aggregate results to
	// the LLM provider: mounted only with llm.api_key (and, for ask, the
	// llm.ask_enabled consent).
	{Method: POST, Path: "/v1/merchant/metrics/ask", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantMetricsRead, When: FeatureMetricsAsk,
		Request: Untyped{}, Responses: []Reply{{200, dashboard.AskResult{}}}, Errors: codes("invalid_param", "rate_limit_exceeded", "service_unavailable"), Handler: h(handlers.MerchantMetricsAsk)},
	{Method: POST, Path: "/v1/merchant/dashboard/widgets/generate", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantDashboardUpdate, When: FeatureDashboardGeneration,
		Request: Untyped{}, Responses: []Reply{{200, dashboard.GenerateResult{}}}, Errors: codes("dashboard_invalid", "invalid_param", "service_unavailable", "widget_generation_invalid"), Handler: h(handlers.GenerateDashboardWidget)},
}
