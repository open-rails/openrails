package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/router"
)

// opsRoutes is how a merchant watches its books: findings (the one inbox),
// worker health, metrics, the dashboard and host events.
var opsRoutes = []Route{
	{Method: GET, Path: "/v1/app/events", Group: App, Auth: AuthApplication, Permission: needEvents, Name: "ListEvents", Query: billing.EventListParams{}, Responses: []Reply{{200, billing.EventPage{}}}, Errors: codes("invalid_cursor", "invalid_query", "invalid_param", "event_cursor_expired"), Handler: h(handlers.ListEvents),
		Doc: "ListEvents reads retained facts of the handle's merchant. Reading starts no worker and changes no " +
			"delivery state. Persist NextCursor after processing."},
	{Method: GET, Path: "/v1/merchant/destructive-holds", Group: MerchantConfig, Auth: AuthMerchant, Name: "ListDestructiveHolds", Level: LevelRead, Query: billing.DestructiveHoldListParams{}, Responses: []Reply{{200, billing.ListPage[billing.DestructiveHold]{}}}, Errors: codes("invalid_cursor", "service_unavailable"), Handler: h(handlers.ListDestructiveHolds),
		Doc: "ListDestructiveHolds lists incident episodes that originated at this merchant."},
	{Method: POST, Path: "/v1/merchant/destructive-holds/{id}/clear", Group: MerchantConfig, Auth: AuthMerchant, Name: "ClearDestructiveHold", Level: LevelUpdate, Request: billing.ClearDestructiveHoldParams{}, Responses: []Reply{{200, billing.DestructiveHold{}}}, Errors: codes("invalid_param", "permission_required", "resource_not_found", "resource_conflict", "service_unavailable"), Bind: incidentClear,
		Args: []any{billing.DestructiveHoldID{}},
		Doc: "ClearDestructiveHold clears an incident after review, recording who and why. It needs a person other " +
			"than the one whose action opened it, holding merchant configuration authority and " +
			"root:admin-rate-limits:unlock, or for the merchant's contact-erasure hold " +
			"merchant:provisioning:manage, whose waiting erasures then run: over HTTP a self-acting human's own " +
			"credential; in process the handle ActingFor names them, once the host has authenticated them."},

	{Method: GET, Path: "/v1/admin/access", Group: Access, Auth: AuthSignedIn, Name: "GetAdminAccess",
		Doc: "GetAdminAccess reports what the caller may use of each staff route group at the merchant and its " +
			"effective programmatic permissions. An engine's handle is the merchant's owner and holds every " +
			"group.",
		Responses: []Reply{{200, billing.AdminAccess{}}}, Bind: adminAccess},
	{Method: POST, Path: "/v1/metrics/query", Group: Metrics, Auth: AuthMerchant, Name: "QueryMetrics",
		Doc:     "QueryMetrics runs one metrics query.",
		Request: billing.QueryMetricsParams{}, Responses: []Reply{{200, billing.MetricsResult{}}}, Errors: codes("metrics_query_invalid", "service_unavailable"), Handler: h(handlers.MerchantMetricsQuery)},
	{Method: GET, Path: "/v1/metrics/schema", Group: Metrics, Auth: AuthMerchant, Name: "GetMetricsSchema",
		Doc:       "GetMetricsSchema lists every measure, dimension and grain a query may use.",
		Responses: []Reply{{200, billing.MetricsSchema{}}}, Handler: h(handlers.MerchantMetricsSchema)},
	{Method: GET, Path: "/v1/metrics/dispute-programs", Group: Metrics, Auth: AuthMerchant, Name: "ListDisputePrograms",
		Doc: "ListDisputePrograms is each card PSP's standing in the dispute monitoring programs for the month containing " +
			"params.At (now when unset): the counts, the ratio so far and projected over the month, the level reached and " +
			"the thresholds in force.",
		Query: billing.DisputeProgramListParams{}, Responses: []Reply{{200, billing.ListPage[billing.DisputeStanding]{}}}, Errors: codes("invalid_param", "service_unavailable"), Handler: h(handlers.ListDisputePrograms)},
	{Method: GET, Path: "/v1/metrics/dashboard", Group: Metrics, Auth: AuthMerchant, Name: "GetDashboard",
		Doc:       "GetDashboard returns the merchant's widget layout.",
		Responses: []Reply{{200, billing.Dashboard{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.GetMerchantDashboard)},
	{Method: PUT, Path: "/v1/merchant/dashboard", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "SetDashboard",
		Doc:     "SetDashboard replaces the merchant's widget layout.",
		Request: billing.SetDashboardParams{}, Responses: []Reply{{200, billing.Dashboard{}}}, Errors: codes("dashboard_invalid", "service_unavailable"), Handler: h(handlers.PutMerchantDashboard)},
	{Method: GET, Path: "/v1/merchant/findings", Group: MerchantConfig, Auth: AuthMerchant, Name: "ListFindings", Level: LevelRead,
		Doc: "ListFindings pages the findings queue: open findings unless params.Status names another, most severe " +
			"first, then oldest. params.Type is one finding type or a prefix ending in \".*\" (\"pull.*\" lists the " +
			"pull's proposals).",
		Query: billing.FindingListParams{}, Responses: []Reply{{200, billing.ListPage[billing.Finding]{}}}, Errors: codes("invalid_cursor", "service_unavailable"), Handler: h(handlers.AdminListFindings)},
	{Method: GET, Path: "/v1/merchant/findings/{id}", Group: MerchantConfig, Auth: AuthMerchant, Name: "GetFinding", Level: LevelRead,
		Args:      []any{billing.FindingID{}},
		Doc:       "GetFinding returns one finding with its evidence and recommendation.",
		Responses: []Reply{{200, billing.Finding{}}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.AdminGetFinding)},
	// Proposals: pull.* findings a person approves or dismisses.
	{Method: GET, Path: "/v1/merchant/proposals", Group: MerchantConfig, Auth: AuthMerchant, Name: "ListProposals", Level: LevelRead,
		Doc: "ListProposals lists the merchant's proposals: open pull.* findings unless Status is set, most severe " +
			"first. Pull names the PSP import whose pull raised them.",
		Query: billing.ProposalListParams{}, Responses: []Reply{{200, billing.ListPage[billing.Finding]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "service_unavailable"), Handler: h(handlers.ListProposals)},
	{Method: POST, Path: "/v1/merchant/proposals/approve", Group: MerchantConfig, Auth: AuthMerchant, Name: "ApproveProposals", Level: LevelUpdate, Person: true, IdempotencyKey: true,
		Doc: "ApproveProposals applies 1 to 100 proposals, each in its own transaction, under one approval run " +
			"that `openrails undo-run` reverses. Only a person signed in, or the in-process host, approves; " +
			"a change that refunds, revokes or cancels also needs merchant:customer:manage. idempotencyKey is " +
			"required: a retry with it answers the first call's results.",
		Request: billing.ApproveProposalsParams{}, Responses: []Reply{{200, billing.ProposalResults{}}}, Errors: codes("finding_not_actionable", "idempotency_key_in_use", "idempotency_key_required", "idempotency_key_reused", "invalid_param", "person_required", "service_unavailable"), Bind: func(e *Env) router.Handler { return h(handlers.ApproveProposals(e.staffCan)) }},
	{Method: POST, Path: "/v1/merchant/proposals/dismiss", Group: MerchantConfig, Auth: AuthMerchant, Name: "DismissProposals", Level: LevelUpdate, Person: true,
		Doc: "DismissProposals closes 1 to 100 proposals without applying them; notes are required. Only a person " +
			"signed in, or the in-process host, dismisses.",
		Request: billing.DismissProposalsParams{}, Responses: []Reply{{200, billing.ProposalResults{}}}, Errors: codes("invalid_param", "person_required", "service_unavailable"), Handler: h(handlers.DismissProposals)},
	{Method: POST, Path: "/v1/merchant/findings/{id}/resolve", Group: MerchantConfig, Auth: AuthMerchant, Name: "ResolveFinding", Level: LevelUpdate,
		Args: []any{billing.FindingID{}},
		Doc: "ResolveFinding approves (runs the recommendation) or ignores one open finding. Approving a " +
			"refund, cancellation or product-access recommendation also needs what that action's own route " +
			"needs: merchant:customer:manage.",
		Request: billing.ResolveFindingParams{}, Responses: []Reply{{200, billing.FindingResolution{}}}, Errors: codes("finding_action_failed", "finding_not_actionable", "finding_requires_merchant_config", "idempotency_key_reused", "invalid_param", "payment_not_found", "payment_not_refundable", "provider_cancel_held", "rebill_terms_committed", "refund_failed", "refund_rail_unavailable", "refund_unsupported", "resource_conflict", "resource_not_found", "service_unavailable"), Bind: func(e *Env) router.Handler { return h(handlers.AdminResolveFinding(e.staffCan)) }},
}
