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
		Args: []any{""},
		Doc: "GetAdmission reads an admitted request and its current hold state without replaying admission. " +
			"Denied or no-longer-retained requests are not found.",
		Responses: []Reply{{200, billing.Admission{}}}, Errors: codes("invalid_param", "admission_not_found", "service_unavailable"), Handler: h(handlers.GetAdmission)},
	{Method: GET, Path: "/v1/app/admissions", Group: App, Auth: AuthApplication, Permission: needUsageRead, Name: "ListAdmissions",
		Doc:   "ListAdmissions reads a page of retained admitted requests, newest first.",
		Query: billing.AdmissionListParams{}, Responses: []Reply{{200, billing.ListPage[billing.Admission]{}}},
		Errors: codes("invalid_cursor", "invalid_query", "currency_unsupported"), Handler: h(handlers.ListAdmissions)},
	{Method: POST, Path: "/v1/app/admissions", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "Admit",
		Doc: "Admit decides admissions, one verdict per request in order. A refused request is a verdict, not an " +
			"error.",
		Request: billing.AdmitBatchParams{}, Responses: []Reply{{200, billing.AdmitBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.Admit)},
	{Method: POST, Path: "/v1/app/admissions/capture", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "CaptureAdmissions",
		Doc: "CaptureAdmissions settles 1 to billing.MaxAdmissionBatchItems admitted requests, one result per item " +
			"in order: each is captured or refused on its own. A capture that names its usage is priced by the " +
			"admission's pinned rate cards; above the hold the rest is owed. An exact retry answers the first " +
			"receipt; changed terms are billing.ErrIdempotencyKeyReused.",
		Request: billing.CaptureAdmissionBatchParams{}, Responses: []Reply{{200, billing.CaptureBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.CaptureAdmissions)},
	{Method: POST, Path: "/v1/app/admissions/release", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "ReleaseAdmissions",
		Doc: "ReleaseAdmissions frees the holds of 1 to billing.MaxAdmissionBatchItems admissions whose work " +
			"failed, one result per request id in order: each is released or refused on its own. Releasing a " +
			"released admission answers it.",
		Request: billing.ReleaseAdmissionBatchParams{}, Responses: []Reply{{200, billing.AdmissionBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ReleaseAdmissions)},
	{Method: POST, Path: "/v1/app/admissions/extend", Group: App, Auth: AuthApplication, Permission: needUsage, Name: "ExtendAdmissions",
		Doc: "ExtendAdmissions moves 1 to billing.MaxAdmissionBatchItems open holds' deadlines later, one result " +
			"per item in order. An item refused with hold_not_found was captured, released or lapsed. New work " +
			"needs a new request ID; replaying the old request never reopens its hold.",
		Request: billing.ExtendAdmissionBatchParams{}, Responses: []Reply{{200, billing.AdmissionBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ExtendAdmissions)},
	{Method: GET, Path: "/v1/me/usage", Group: Customer, Auth: AuthCustomer,
		Query: billing.GetUsageParams{}, Responses: []Reply{{200, billing.Usage{}}}, Errors: codes("currency_unsupported", "invalid_cursor"), Handler: h(handlers.GetMyUsage)},
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
