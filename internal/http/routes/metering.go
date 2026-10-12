package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// admissionErrors are the codes of a route addressed to one admission.
var admissionErrors = []string{"admission_not_found", billing.CodeServiceCredentialCustomerScopeDenied}

func withAdmission(list ...string) []string { return codes(append(list, admissionErrors...)...) }

// usageQuery is a usage report's query: a page of its groups.
var usageQuery = params(text("currency"), text("from"), text("group_by"), pageParams, text("to"))

// meteringRoutes is spend as it happens: admissions and their holds, usage
// events (failed usage included) and the customer's own report, the charges
// that run at a rate until they are stopped, and the provider operations a
// host settles against its own upstream costs.
var meteringRoutes = []Route{
	{Method: GET, Path: "/v1/app/admissions/{request_id}", Group: App, Auth: AuthApplication, Permission: needUsageRead, Name: "GetAdmission",
		Responses: []Reply{{200, billing.Admission{}}}, Errors: withAdmission("invalid_param"), Handler: h(handlers.GetAdmission)},
	{Method: GET, Path: "/v1/app/admissions", Group: App, Auth: AuthApplication, Permission: needUsageRead, Name: "ListAdmissions",
		Query: params(cursorPage, text("customer_id"), text("currency"), text("state"), text("from"), text("to")), Responses: []Reply{{200, billing.ListPage[billing.Admission]{}}},
		Errors: codes("invalid_cursor", "invalid_query", "currency_unsupported"), Handler: h(handlers.ListAdmissions)},
	{Method: GET, Path: "/v1/app/usage-events/{id}", Group: App, Auth: AuthApplication, Permission: needUsageRead, Name: "GetUsageEvent",
		Responses: []Reply{{200, billing.UsageEvent{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.GetUsageEvent)},
	{Method: GET, Path: "/v1/app/usage-events", Group: App, Auth: AuthApplication, Permission: needUsageRead, Name: "ListUsageEvents",
		Query: params(cursorPage, text("customer_id"), text("currency"), text("event_type"), text("source"), text("source_id"), text("outcome"), text("from"), text("to")), Responses: []Reply{{200, billing.ListPage[billing.UsageEvent]{}}},
		Errors: codes("invalid_cursor", "invalid_query", "currency_unsupported"), Handler: h(handlers.ListUsageEvents)},
	{Method: POST, Path: "/v1/app/admissions", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "Admit", IdempotencyKey: true,
		Request: billing.AdmitBatchParams{}, Responses: []Reply{{200, billing.AdmitBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.Admit)},
	{Method: POST, Path: "/v1/app/admissions/{request_id}/capture", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "CaptureAdmission", IdempotencyKey: true,
		Request: billing.CaptureAdmissionParams{}, Responses: []Reply{{200, billing.CaptureReceipt{}}},
		Errors: withAdmission("idempotency_key_reused", "insufficient_credits", "invalid_param"), Handler: h(handlers.CaptureAdmission)},
	{Method: POST, Path: "/v1/app/admissions/release", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "ReleaseAdmissions", IdempotencyKey: true,
		Request: billing.ReleaseAdmissionBatchParams{}, Responses: []Reply{{200, billing.AdmissionBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ReleaseAdmissions)},
	{Method: POST, Path: "/v1/app/admissions/extend", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "ExtendAdmissions", IdempotencyKey: true,
		Request: billing.ExtendAdmissionBatchParams{}, Responses: []Reply{{200, billing.AdmissionBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ExtendAdmissions)},
	{Method: POST, Path: "/v1/app/usage-events", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "RecordUsage", IdempotencyKey: true,
		Request: billing.RecordUsageBatchParams{}, Responses: []Reply{{200, billing.RecordUsageBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.RecordUsage)},
	{Method: GET, Path: "/v1/me/usage", Group: Customer, Auth: AuthCustomer,
		Query: usageQuery, Responses: []Reply{{200, billing.Usage{}}}, Errors: codes("currency_unsupported", "invalid_cursor"), Handler: h(handlers.GetMyUsage)},
	{Method: POST, Path: "/v1/app/running-charges", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "StartRunningCharge", IdempotencyKey: true,
		Request: billing.StartRunningChargeParams{}, Responses: []Reply{{201, billing.RunningCharge{}}, {200, billing.RunningCharge{}}},
		Errors: codes("currency_unsupported", "idempotency_key_reused", "insufficient_credits", "invalid_param"), Handler: h(handlers.StartRunningCharge)},
	{Method: POST, Path: "/v1/app/running-charges/{reference}/stop", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "StopRunningCharge", IdempotencyKey: true,
		Request: billing.StopRunningChargeParams{}, Responses: []Reply{{200, billing.RunningCharge{}}},
		Errors: codes("idempotency_key_reused", "invalid_param", "running_charge_not_found"), Handler: h(handlers.StopRunningCharge)},
	{Method: GET, Path: "/v1/app/running-charges/{reference}", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "GetRunningCharge",
		Responses: []Reply{{200, billing.RunningCharge{}}}, Errors: codes("running_charge_not_found"), Handler: h(handlers.GetRunningCharge)},
	{Method: GET, Path: "/v1/app/running-charges", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "ListRunningCharges",
		Query: params(cursorPage, text("customer_id"), Param{Name: "depleted", Kind: "boolean"}, text("state")), Responses: []Reply{{200, billing.ListPage[billing.RunningCharge]{}}},
		Errors: codes("invalid_cursor", "invalid_query"), Handler: h(handlers.ListRunningCharges)},
}
