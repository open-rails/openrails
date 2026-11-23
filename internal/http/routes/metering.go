package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// meteringRoutes is spend as it happens: admissions and their holds, the
// customer's own usage report, the charges that run at a rate until they are
// stopped, and the provider operations a host settles against its own
// upstream costs. Usage itself arrives in lease syncs (limits.go).
var meteringRoutes = []Route{
	{Method: GET, Path: "/v1/app/admissions/{request_id}", Group: App, Auth: AuthApplication, Permission: needUsageRead, Name: "GetAdmission",
		Responses: []Reply{{200, billing.Admission{}}}, Errors: codes("invalid_param", "admission_not_found", "service_unavailable"), Handler: h(handlers.GetAdmission)},
	{Method: GET, Path: "/v1/app/admissions", Group: App, Auth: AuthApplication, Permission: needUsageRead, Name: "ListAdmissions",
		Query: billing.AdmissionListParams{}, Responses: []Reply{{200, billing.ListPage[billing.Admission]{}}},
		Errors: codes("invalid_cursor", "invalid_query", "currency_unsupported"), Handler: h(handlers.ListAdmissions)},
	{Method: POST, Path: "/v1/app/admissions", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "Admit",
		Request: billing.AdmitBatchParams{}, Responses: []Reply{{200, billing.AdmitBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.Admit)},
	{Method: POST, Path: "/v1/app/admissions/capture", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "CaptureAdmissions",
		Request: billing.CaptureAdmissionBatchParams{}, Responses: []Reply{{200, billing.CaptureBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.CaptureAdmissions)},
	{Method: POST, Path: "/v1/app/admissions/release", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "ReleaseAdmissions",
		Request: billing.ReleaseAdmissionBatchParams{}, Responses: []Reply{{200, billing.AdmissionBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ReleaseAdmissions)},
	{Method: POST, Path: "/v1/app/admissions/extend", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "ExtendAdmissions",
		Request: billing.ExtendAdmissionBatchParams{}, Responses: []Reply{{200, billing.AdmissionBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ExtendAdmissions)},
	{Method: GET, Path: "/v1/me/usage", Group: Customer, Auth: AuthCustomer,
		Query: billing.GetUsageParams{}, Responses: []Reply{{200, billing.Usage{}}}, Errors: codes("currency_unsupported", "invalid_cursor"), Handler: h(handlers.GetMyUsage)},
	{Method: POST, Path: "/v1/app/running-charges", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "StartRunningCharge",
		Request: billing.StartRunningChargeParams{}, Responses: []Reply{{201, billing.RunningCharge{}}, {200, billing.RunningCharge{}}},
		Errors: codes("currency_unsupported", "idempotency_key_reused", "insufficient_credits", "invalid_param"), Handler: h(handlers.StartRunningCharge)},
	{Method: POST, Path: "/v1/app/running-charges/{reference}/stop", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "StopRunningCharge",
		Request: billing.StopRunningChargeParams{}, Responses: []Reply{{200, billing.RunningCharge{}}},
		Errors: codes("idempotency_key_reused", "invalid_param", "running_charge_not_found"), Handler: h(handlers.StopRunningCharge)},
	{Method: GET, Path: "/v1/app/running-charges/{reference}", Group: App, Auth: AuthApplication, Permission: needUsageRead, Name: "GetRunningCharge",
		Responses: []Reply{{200, billing.RunningCharge{}}}, Errors: codes("running_charge_not_found"), Handler: h(handlers.GetRunningCharge)},
	{Method: GET, Path: "/v1/app/running-charges", Group: App, Auth: AuthApplication, Permission: needUsageRead, Name: "ListRunningCharges",
		Query: billing.RunningChargeListParams{}, Responses: []Reply{{200, billing.ListPage[billing.RunningCharge]{}}},
		Errors: codes("invalid_cursor", "invalid_query"), Handler: h(handlers.ListRunningCharges)},
}
