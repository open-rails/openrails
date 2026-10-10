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
	{Method: POST, Path: "/v1/admin/admissions", Group: Admin, Auth: AuthMerchant, Name: "Admit", Level: LevelWrite, Sensitive: true,
		Request: billing.AdmitBatchParams{}, Responses: []Reply{{200, billing.AdmitBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.Admit)},
	{Method: GET, Path: "/v1/admin/admissions/{request_id}", Group: Admin, Auth: AuthMerchant, Name: "GetAdmission", Level: LevelRead,
		Responses: []Reply{{200, billing.Admission{}}}, Errors: withAdmission(), Handler: h(handlers.GetAdmission)},
	{Method: POST, Path: "/v1/admin/admissions/{request_id}/capture", Group: Admin, Auth: AuthMerchant, Name: "CaptureAdmission", Level: LevelWrite, Sensitive: true,
		Request: billing.CaptureAdmissionParams{}, Responses: []Reply{{200, billing.CaptureReceipt{}}},
		Errors: withAdmission("idempotency_key_reused", "insufficient_credits", "invalid_param"), Handler: h(handlers.CaptureAdmission)},
	{Method: POST, Path: "/v1/admin/admissions/release", Group: Admin, Auth: AuthMerchant, Name: "ReleaseAdmissions", Level: LevelWrite, Sensitive: true,
		Request: billing.ReleaseAdmissionBatchParams{}, Responses: []Reply{{200, billing.AdmissionBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ReleaseAdmissions)},
	{Method: POST, Path: "/v1/admin/admissions/extend", Group: Admin, Auth: AuthMerchant, Name: "ExtendAdmissions", Level: LevelWrite, Sensitive: true,
		Request: billing.ExtendAdmissionBatchParams{}, Responses: []Reply{{200, billing.AdmissionBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ExtendAdmissions)},
	{Method: POST, Path: "/v1/admin/usage-events", Group: Admin, Auth: AuthMerchant, Name: "RecordUsage", Level: LevelWrite, Sensitive: true,
		Request: billing.RecordUsageBatchParams{}, Responses: []Reply{{200, billing.RecordUsageBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.RecordUsage)},
	{Method: GET, Path: "/v1/me/usage", Group: Customer, Auth: AuthCustomer,
		Query: usageQuery, Responses: []Reply{{200, billing.Usage{}}}, Errors: codes("currency_unsupported"), Handler: h(handlers.GetMyUsage)},
	{Method: POST, Path: "/v1/admin/provider-operations", Group: Admin, Auth: AuthMerchant, Name: "OpenProviderOperation", Level: LevelWrite, Sensitive: true,
		Request: billing.OpenProviderOperationParams{}, Responses: []Reply{{201, billing.ProviderOperation{}}, {200, billing.ProviderOperation{}}},
		Errors: codes("insufficient_credits", "invalid_param", "provider_operation_conflict"), Handler: h(handlers.ServiceOpenProviderOperation)},
	{Method: GET, Path: "/v1/admin/provider-operations", Group: Admin, Auth: AuthMerchant, Name: "ListProviderOperations", Level: LevelRead,
		Query: params(cursorPage, Param{Name: "refused", Kind: "boolean"}, text("state")), Responses: []Reply{{200, billing.ListPage[billing.ProviderOperation]{}}},
		Errors: codes("invalid_cursor", "invalid_query"), Handler: h(handlers.ServiceListProviderOperations)},
	{Method: GET, Path: "/v1/admin/provider-operations/{operation_id}", Group: Admin, Auth: AuthMerchant, Name: "GetProviderOperation", Level: LevelRead,
		Responses: []Reply{{200, billing.ProviderOperation{}}}, Errors: codes("invalid_param", "provider_operation_not_found"), Handler: h(handlers.ServiceGetProviderOperation)},
	{Method: POST, Path: "/v1/admin/provider-operations/{operation_id}/increment", Group: Admin, Auth: AuthMerchant, Name: "IncrementProviderOperation", Level: LevelWrite, Sensitive: true,
		Request: billing.IncrementProviderOperationParams{}, Responses: []Reply{{200, billing.ProviderOperation{}}},
		Errors: codes("insufficient_credits", "invalid_param", "provider_operation_conflict", "provider_operation_not_found", "provider_operation_not_open", "provider_operation_refused"), Handler: h(handlers.ServiceIncrementProviderOperation)},
	{Method: POST, Path: "/v1/admin/provider-operations/{operation_id}/release", Group: Admin, Auth: AuthMerchant, Name: "ReleaseProviderOperation", Level: LevelWrite, Sensitive: true,
		Request: billing.ReleaseProviderOperationParams{}, Responses: []Reply{{200, billing.ProviderOperation{}}},
		Errors: codes("invalid_param", "provider_operation_conflict", "provider_operation_has_billing_evidence", "provider_operation_not_found", "provider_operation_not_open", "provider_operation_refused"), Handler: h(handlers.ServiceReleaseProviderOperation)},
	{Method: POST, Path: "/v1/admin/provider-operations/{operation_id}/observations", Group: Admin, Auth: AuthMerchant, Name: "RecordProviderBillingObservation", Level: LevelWrite, Sensitive: true,
		Request: billing.RecordProviderBillingObservationParams{}, Responses: []Reply{{200, billing.ProviderOperation{}}},
		Errors: codes("insufficient_credits", "invalid_param", "provider_billing_observation_conflict", "provider_operation_not_found", "provider_operation_not_open", "provider_operation_refused"), Handler: h(handlers.ServiceRecordProviderBillingObservation)},
	{Method: POST, Path: "/v1/admin/provider-operations/{operation_id}/close", Group: Admin, Auth: AuthMerchant, Name: "CloseProviderOperation", Level: LevelWrite, Sensitive: true,
		Request: billing.CloseProviderOperationParams{}, Responses: []Reply{{200, billing.ProviderOperation{}}},
		Errors: codes("invalid_param", "provider_operation_conflict", "provider_operation_not_found", "provider_operation_not_open", "provider_operation_not_refused"), Handler: h(handlers.ServiceCloseProviderOperation)},
}
