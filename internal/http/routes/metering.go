package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/service"
)

// meteringRoutes is spend as it happens: admissions and their holds, usage
// reports and rollups, and the provider operations a host settles against
// its own upstream costs.
var meteringRoutes = []Route{
	{Method: POST, Path: "/v1/merchant/admissions", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Request: handlers.ServiceAdmitBatchRequest{}, Responses: []Reply{{200, Untyped{}}}, Errors: codes("idempotency_key_reused", "invalid_param"), Handler: h(handlers.ServiceAdmitBatch)},
	{Method: POST, Path: "/v1/merchant/wasted-spend", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Request: handlers.ServiceReportWastedSpendRequest{}, Responses: []Reply{{200, service.WastedSpendResult{}}}, Errors: codes("authentication_required", "currency_unsupported", "idempotency_key_reused", "invalid_param"), Handler: h(handlers.ServiceReportWastedSpend)},
	{Method: POST, Path: "/v1/merchant/admissions/{id}/capture", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Request: handlers.ServiceCaptureRequest{}, Responses: []Reply{{200, billing.CaptureReceipt{}}}, Errors: codes("authentication_required", "idempotency_key_reused", "insufficient_credits", "invalid_param", "resource_not_found"), Handler: h(handlers.ServiceCaptureHold)},
	{Method: POST, Path: "/v1/merchant/admissions/{id}/release", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "invalid_param", "resource_conflict", "resource_not_found"), Handler: h(handlers.ServiceReleaseHold)},
	{Method: POST, Path: "/v1/merchant/admissions/{id}/extend", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Request: handlers.ServiceExtendHoldRequest{}, Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "invalid_param", "resource_not_found"), Handler: h(handlers.ServiceExtendHold)},
	{Method: POST, Path: "/v1/merchant/provider-operations", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Request: Untyped{}, Responses: []Reply{{200, Untyped{}}}, Errors: codes("insufficient_credits", "invalid_param"), Handler: h(handlers.ServiceOpenOperationAuthorization)},
	{Method: GET, Path: "/v1/merchant/provider-operations/{operation_id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantUsageRead,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("insufficient_credits", "invalid_param"), Handler: h(handlers.ServiceGetOperationAuthorization)},
	{Method: POST, Path: "/v1/merchant/provider-operations/{operation_id}/release", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Request: Untyped{}, Responses: []Reply{{200, Untyped{}}}, Errors: codes("insufficient_credits", "invalid_param"), Handler: h(handlers.ServiceReleaseOperationAuthorization)},
	{Method: POST, Path: "/v1/merchant/provider-operations/{operation_id}/observations", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Request: Untyped{}, Responses: []Reply{{200, Untyped{}}}, Errors: codes("insufficient_credits", "invalid_param"), Handler: h(handlers.ServiceRecordProviderBillingObservation)},
	{Method: GET, Path: "/v1/merchant/provider-operations/{operation_id}/qualification", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantUsageRead,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("insufficient_credits", "invalid_param"), Handler: h(handlers.ServiceGetProviderBillingQualification)},
	{Method: POST, Path: "/v1/merchant/usage/report", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantAdmissionsCreate,
		Request: handlers.ServiceRecordUsageRequest{}, Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "currency_unsupported", "idempotency_key_reused", "invalid_param"), Handler: h(handlers.ServiceRecordUsage)},
	{Method: POST, Path: "/v1/merchant/usage/rollup", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantUsageRead,
		Request: handlers.ServiceUsageRollupRequest{}, Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "currency_unsupported", "invalid_param"), Handler: h(handlers.ServiceUsageRollup)},
	{Method: POST, Path: "/v1/merchant/usage/resource-revenue", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantUsageRead,
		Request: handlers.ServiceEndpointRevenueRequest{}, Responses: []Reply{{200, billing.ResourceRevenueResponse{}}}, Errors: codes("currency_unsupported", "invalid_param"), Handler: h(handlers.ServiceResourceRevenue)},
	{Method: GET, Path: "/v1/me/usage", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: params(text("currency"), text("from"), text("to")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "currency_unsupported", "invalid_param"), Handler: h(handlers.GetMyUsage)},
	{Method: GET, Path: "/v1/customers/{customer_id}/usage", Group: Treasury, Auth: AuthCustomerGrant, Perm: billing.CustomerBalanceRead,
		Query: params(text("currency"), text("from"), text("to")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "currency_unsupported", "invalid_param"), Handler: h(handlers.GetMyUsage)},
}
