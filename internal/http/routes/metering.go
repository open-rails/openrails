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
	{Method: POST, Path: "/v1/merchant/admissions", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Request: billing.AdmitBatchParams{}, Responses: []Reply{{200, billing.AdmitBatchResult{}}}, Errors: codes("invalid_param"), Handler: h(handlers.Admit)},
	{Method: GET, Path: "/v1/merchant/admissions/{request_id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantUsageRead,
		Responses: []Reply{{200, billing.Admission{}}}, Errors: withAdmission(), Handler: h(handlers.GetAdmission)},
	{Method: POST, Path: "/v1/merchant/admissions/{request_id}/capture", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Request: billing.CaptureAdmissionParams{}, Responses: []Reply{{200, billing.CaptureReceipt{}}},
		Errors: withAdmission("idempotency_key_reused", "insufficient_credits", "invalid_param"), Handler: h(handlers.CaptureAdmission)},
	{Method: POST, Path: "/v1/merchant/admissions/{request_id}/release", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Responses: []Reply{{200, billing.Admission{}}}, Errors: withAdmission("admission_captured"), Handler: h(handlers.ReleaseAdmission)},
	{Method: POST, Path: "/v1/merchant/admissions/{request_id}/extend", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Request: billing.ExtendAdmissionParams{}, Responses: []Reply{{200, billing.Admission{}}}, Errors: withAdmission("hold_not_found", "invalid_param"), Handler: h(handlers.ExtendAdmission)},
	{Method: POST, Path: "/v1/merchant/wasted-spend", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Request: billing.ReportWastedSpendParams{}, Responses: []Reply{{200, billing.WastedSpendReport{}}},
		Errors: codes("currency_unsupported", "idempotency_key_reused", "invalid_param", billing.CodeServiceCredentialCustomerScopeDenied), Handler: h(handlers.ReportWastedSpend)},
	{Method: POST, Path: "/v1/merchant/usage-events", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Request: billing.RecordUsageParams{}, Responses: []Reply{{201, billing.UsageEvent{}}, {200, billing.UsageEvent{}}},
		Errors: codes("currency_unsupported", "idempotency_key_reused", "invalid_param", billing.CodeServiceCredentialCustomerScopeDenied), Handler: h(handlers.RecordUsageEvent)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/usage", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantUsageRead,
		Query: usageQuery, Responses: []Reply{{200, billing.Usage{}}}, Errors: withCustomer("currency_unsupported"), Handler: h(handlers.GetCustomerUsage)},
	{Method: GET, Path: "/v1/me/usage", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: usageQuery, Responses: []Reply{{200, billing.Usage{}}}, Errors: codes("currency_unsupported"), Handler: h(handlers.GetMyUsage)},
	{Method: POST, Path: "/v1/merchant/provider-operations", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Request: billing.OpenOperationAuthorizationParams{}, Responses: []Reply{{200, billing.OperationAuthorization{}}},
		Errors: codes("insufficient_credits", "invalid_param", "operation_authorization_conflict"), Handler: h(handlers.ServiceOpenOperationAuthorization)},
	{Method: GET, Path: "/v1/merchant/provider-operations/{operation_id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantUsageRead,
		Responses: []Reply{{200, billing.OperationAuthorization{}}}, Errors: codes("invalid_param", "operation_authorization_not_found"), Handler: h(handlers.ServiceGetOperationAuthorization)},
	{Method: POST, Path: "/v1/merchant/provider-operations/{operation_id}/release", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Request: billing.ReleaseOperationAuthorizationParams{}, Responses: []Reply{{200, billing.OperationAuthorization{}}},
		Errors: codes("invalid_param", "operation_authorization_conflict", "operation_authorization_has_billing_evidence", "operation_authorization_not_found", "operation_authorization_not_open"), Handler: h(handlers.ServiceReleaseOperationAuthorization)},
	{Method: POST, Path: "/v1/merchant/provider-operations/{operation_id}/observations", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Request: billing.RecordProviderBillingObservationParams{}, Responses: []Reply{{200, billing.ProviderBillingQualification{}}},
		Errors: codes("insufficient_credits", "invalid_param", "operation_authorization_not_found", "operation_authorization_not_open", "provider_billing_observation_conflict", "provider_billing_qualification_refused"), Handler: h(handlers.ServiceRecordProviderBillingObservation)},
	{Method: GET, Path: "/v1/merchant/provider-operations/{operation_id}/qualification", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantUsageRead,
		Responses: []Reply{{200, billing.ProviderBillingQualification{}}}, Errors: codes("invalid_param", "provider_billing_qualification_not_found"), Handler: h(handlers.ServiceGetProviderBillingQualification)},
}
