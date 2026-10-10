package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// admissionErrors are the codes of a route addressed to one admission.
var admissionErrors = []string{"admission_not_found", billing.CodeServiceCredentialCustomerScopeDenied}

func withAdmission(list ...string) []string { return codes(append(list, admissionErrors...)...) }

// usageQuery is a usage report's query.
var usageQuery = params(text("currency"), text("from"), text("group_by"), text("to"))

// meteringRoutes is spend as it happens: admissions and their holds, usage
// events (failed usage included) and the customer's own report, and the
// provider operations a host settles against its own upstream costs.
var meteringRoutes = []Route{
	{Method: POST, Path: "/v1/app/admissions", Group: App, Auth: AuthApplication, Permission: NeedUsage, Name: "Admit", IdempotencyKey: true,
		Request: billing.AdmitBatchParams{}, Responses: []Reply{{200, billing.AdmitBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.Admit)},
	{Method: POST, Path: "/v1/app/admissions/{request_id}/capture", Group: App, Auth: AuthApplication, Permission: NeedUsage, Name: "CaptureAdmission", IdempotencyKey: true,
		Request: billing.CaptureAdmissionParams{}, Responses: []Reply{{200, billing.CaptureReceipt{}}},
		Errors: withAdmission("idempotency_key_reused", "insufficient_credits", "invalid_param"), Handler: h(handlers.CaptureAdmission)},
	{Method: POST, Path: "/v1/app/admissions/release", Group: App, Auth: AuthApplication, Permission: NeedUsage, Name: "ReleaseAdmissions", IdempotencyKey: true,
		Request: billing.ReleaseAdmissionBatchParams{}, Responses: []Reply{{200, billing.AdmissionBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ReleaseAdmissions)},
	{Method: POST, Path: "/v1/app/admissions/extend", Group: App, Auth: AuthApplication, Permission: NeedUsage, Name: "ExtendAdmissions", IdempotencyKey: true,
		Request: billing.ExtendAdmissionBatchParams{}, Responses: []Reply{{200, billing.AdmissionBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ExtendAdmissions)},
	{Method: POST, Path: "/v1/app/usage-events", Group: App, Auth: AuthApplication, Permission: NeedUsage, Name: "RecordUsage", IdempotencyKey: true,
		Request: billing.RecordUsageBatchParams{}, Responses: []Reply{{200, billing.RecordUsageBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.RecordUsage)},
	{Method: GET, Path: "/v1/me/usage", Group: Customer, Auth: AuthCustomer,
		Query: usageQuery, Responses: []Reply{{200, billing.Usage{}}}, Errors: codes("currency_unsupported"), Handler: h(handlers.GetMyUsage)},
	{Method: POST, Path: "/v1/app/provider-operations", Group: App, Auth: AuthApplication, Permission: NeedCosts, Name: "OpenProviderOperation", IdempotencyKey: true,
		Request: billing.OpenProviderOperationParams{}, Responses: []Reply{{201, billing.ProviderOperation{}}, {200, billing.ProviderOperation{}}},
		Errors: codes("insufficient_credits", "invalid_param", "provider_operation_conflict"), Handler: h(handlers.ServiceOpenProviderOperation)},
	{Method: GET, Path: "/v1/admin/provider-operations", Group: Admin, Auth: AuthMerchant, Name: "ListProviderOperations", Level: LevelRead,
		Query: params(cursorPage, Param{Name: "refused", Kind: "boolean"}, text("state")), Responses: []Reply{{200, billing.ListPage[billing.ProviderOperation]{}}},
		Errors: codes("invalid_cursor", "invalid_query"), Handler: h(handlers.ServiceListProviderOperations)},
	{Method: GET, Path: "/v1/admin/provider-operations/{operation_id}", Group: Admin, Auth: AuthMerchant, Name: "GetProviderOperation", Level: LevelRead,
		Responses: []Reply{{200, billing.ProviderOperation{}}}, Errors: codes("invalid_param", "provider_operation_not_found"), Handler: h(handlers.ServiceGetProviderOperation)},
	{Method: POST, Path: "/v1/app/provider-operations/{operation_id}/increment", Group: App, Auth: AuthApplication, Permission: NeedCosts, Name: "IncrementProviderOperation", IdempotencyKey: true,
		Request: billing.IncrementProviderOperationParams{}, Responses: []Reply{{200, billing.ProviderOperation{}}},
		Errors: codes("insufficient_credits", "invalid_param", "provider_operation_conflict", "provider_operation_not_found", "provider_operation_not_open", "provider_operation_refused"), Handler: h(handlers.ServiceIncrementProviderOperation)},
	{Method: POST, Path: "/v1/app/provider-operations/{operation_id}/release", Group: App, Auth: AuthApplication, Permission: NeedCosts, Name: "ReleaseProviderOperation", IdempotencyKey: true,
		Request: billing.ReleaseProviderOperationParams{}, Responses: []Reply{{200, billing.ProviderOperation{}}},
		Errors: codes("invalid_param", "provider_operation_conflict", "provider_operation_has_billing_evidence", "provider_operation_not_found", "provider_operation_not_open", "provider_operation_refused"), Handler: h(handlers.ServiceReleaseProviderOperation)},
	{Method: POST, Path: "/v1/app/provider-operations/{operation_id}/observations", Group: App, Auth: AuthApplication, Permission: NeedCosts, Name: "RecordProviderBillingObservation", IdempotencyKey: true,
		Request: billing.RecordProviderBillingObservationParams{}, Responses: []Reply{{200, billing.ProviderOperation{}}},
		Errors: codes("insufficient_credits", "invalid_param", "provider_billing_observation_conflict", "provider_operation_not_found", "provider_operation_not_open", "provider_operation_refused"), Handler: h(handlers.ServiceRecordProviderBillingObservation)},
	{Method: POST, Path: "/v1/admin/provider-operations/{operation_id}/close", Group: Admin, Auth: AuthMerchant, Name: "CloseProviderOperation", Level: LevelUpdate, Sensitive: true,
		Request: billing.CloseProviderOperationParams{}, Responses: []Reply{{200, billing.ProviderOperation{}}},
		Errors: codes("invalid_param", "provider_operation_conflict", "provider_operation_not_found", "provider_operation_not_open", "provider_operation_not_refused"), Handler: h(handlers.ServiceCloseProviderOperation)},
}
