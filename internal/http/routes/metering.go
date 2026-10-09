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
// events and reports, wasted spend, and the provider operations a host settles
// against its own upstream costs.
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
	{Method: POST, Path: "/v1/admin/wasted-spend", Group: Admin, Auth: AuthMerchant, Name: "ReportWastedSpend", Level: LevelWrite, Sensitive: true,
		Request: billing.ReportWastedSpendBatchParams{}, Responses: []Reply{{200, billing.ReportWastedSpendBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ReportWastedSpend)},
	{Method: POST, Path: "/v1/admin/usage-events", Group: Admin, Auth: AuthMerchant, Name: "RecordUsage", Level: LevelWrite, Sensitive: true,
		Request: billing.RecordUsageBatchParams{}, Responses: []Reply{{200, billing.RecordUsageBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.RecordUsage)},
	{Method: GET, Path: "/v1/admin/customers/{customer_id}/usage", Group: Admin, Auth: AuthMerchant, Name: "GetUsage", Level: LevelRead,
		Query: usageQuery, Responses: []Reply{{200, billing.Usage{}}}, Errors: withCustomer("currency_unsupported"), Handler: h(handlers.GetCustomerUsage)},
	{Method: GET, Path: "/v1/me/usage", Group: Customer, Auth: AuthCustomer,
		Query: usageQuery, Responses: []Reply{{200, billing.Usage{}}}, Errors: codes("currency_unsupported"), Handler: h(handlers.GetMyUsage)},
	{Method: POST, Path: "/v1/admin/provider-operations", Group: Admin, Auth: AuthMerchant, Name: "OpenOperationAuthorization", Level: LevelWrite, Sensitive: true,
		Request: billing.OpenOperationAuthorizationParams{}, Responses: []Reply{{200, billing.OperationAuthorization{}}},
		Errors: codes("insufficient_credits", "invalid_param", "operation_authorization_conflict"), Handler: h(handlers.ServiceOpenOperationAuthorization)},
	{Method: GET, Path: "/v1/admin/provider-operations", Group: Admin, Auth: AuthMerchant, Name: "ListOperationAuthorizations", Level: LevelRead,
		Query: params(cursorPage, Param{Name: "refused", Kind: "boolean"}, text("state")), Responses: []Reply{{200, billing.ListPage[billing.OperationAuthorization]{}}},
		Errors: codes("invalid_cursor", "invalid_query"), Handler: h(handlers.ServiceListOperationAuthorizations)},
	{Method: GET, Path: "/v1/admin/provider-operations/{operation_id}", Group: Admin, Auth: AuthMerchant, Name: "GetOperationAuthorization", Level: LevelRead,
		Responses: []Reply{{200, billing.OperationAuthorization{}}}, Errors: codes("invalid_param", "operation_authorization_not_found"), Handler: h(handlers.ServiceGetOperationAuthorization)},
	{Method: POST, Path: "/v1/admin/provider-operations/{operation_id}/extend", Group: Admin, Auth: AuthMerchant, Name: "ExtendOperationAuthorization", Level: LevelWrite, Sensitive: true,
		Request: billing.ExtendOperationAuthorizationParams{}, Responses: []Reply{{200, billing.OperationAuthorizationExtension{}}},
		Errors: codes("insufficient_credits", "invalid_param", "operation_authorization_conflict", "operation_authorization_not_found", "operation_authorization_not_open", "provider_billing_qualification_refused"), Handler: h(handlers.ServiceExtendOperationAuthorization)},
	{Method: POST, Path: "/v1/admin/provider-operations/{operation_id}/release", Group: Admin, Auth: AuthMerchant, Name: "ReleaseOperationAuthorization", Level: LevelWrite, Sensitive: true,
		Request: billing.ReleaseOperationAuthorizationParams{}, Responses: []Reply{{200, billing.OperationAuthorization{}}},
		Errors: codes("invalid_param", "operation_authorization_conflict", "operation_authorization_has_billing_evidence", "operation_authorization_not_found", "operation_authorization_not_open", "provider_billing_qualification_refused"), Handler: h(handlers.ServiceReleaseOperationAuthorization)},
	{Method: POST, Path: "/v1/admin/provider-operations/{operation_id}/observations", Group: Admin, Auth: AuthMerchant, Name: "RecordProviderBillingObservation", Level: LevelWrite, Sensitive: true,
		Request: billing.RecordProviderBillingObservationParams{}, Responses: []Reply{{200, billing.ProviderBillingQualification{}}},
		Errors: codes("insufficient_credits", "invalid_param", "operation_authorization_not_found", "operation_authorization_not_open", "provider_billing_observation_conflict", "provider_billing_qualification_refused"), Handler: h(handlers.ServiceRecordProviderBillingObservation)},
	{Method: GET, Path: "/v1/admin/provider-operations/{operation_id}/qualification", Group: Admin, Auth: AuthMerchant, Name: "GetProviderBillingQualification", Level: LevelRead,
		Responses: []Reply{{200, billing.ProviderBillingQualification{}}}, Errors: codes("invalid_param", "provider_billing_qualification_not_found"), Handler: h(handlers.ServiceGetProviderBillingQualification)},
	{Method: POST, Path: "/v1/admin/provider-operations/{operation_id}/resolution", Group: Admin, Auth: AuthMerchant, Name: "ResolveProviderBillingQualification", Level: LevelWrite, Sensitive: true,
		Request: billing.ResolveProviderBillingQualificationParams{}, Responses: []Reply{{200, billing.ProviderBillingQualification{}}},
		Errors: codes("invalid_param", "operation_authorization_not_found", "operation_authorization_not_open", "provider_billing_qualification_not_found", "provider_billing_qualification_not_refused", "provider_billing_resolution_conflict"), Handler: h(handlers.ServiceResolveProviderBillingQualification)},
	{Method: POST, Path: "/v1/admin/provider-operations/{operation_id}/refusal", Group: Admin, Auth: AuthMerchant, Name: "RefuseProviderBillingQualification", Level: LevelWrite, Sensitive: true,
		Request: billing.RefuseProviderBillingQualificationParams{}, Responses: []Reply{{200, billing.OperationAuthorization{}}},
		Errors: codes("invalid_param", "operation_authorization_not_found", "operation_authorization_not_open", "provider_billing_refusal_conflict"), Handler: h(handlers.ServiceRefuseProviderBillingQualification)},
	{Method: POST, Path: "/v1/admin/provider-operations/{operation_id}/close", Group: Admin, Auth: AuthMerchant, Name: "CloseOperationAuthorization", Level: LevelWrite, Sensitive: true,
		Request: billing.CloseOperationAuthorizationParams{}, Responses: []Reply{{200, billing.OperationAuthorization{}}},
		Errors: codes("invalid_param", "operation_authorization_not_found", "operation_authorization_not_open", "provider_billing_qualification_not_refused", "provider_billing_resolution_conflict"), Handler: h(handlers.ServiceCloseOperationAuthorization)},
	{Method: GET, Path: "/v1/admin/provider-qualifications", Group: Admin, Auth: AuthMerchant, Name: "ListProviderBillingQualifications", Level: LevelRead,
		Query: params(cursorPage, text("authorization_state"), text("state")), Responses: []Reply{{200, billing.ListPage[billing.ProviderBillingQualification]{}}},
		Errors: codes("invalid_cursor", "invalid_query"), Handler: h(handlers.ServiceListProviderBillingQualifications)},
}
