package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/merchants"
)

// pspsRoutes is a merchant's payment service providers, and the callbacks
// those providers send.
var pspsRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/payment-providers", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPaymentProvidersRead,
		Query: params(text("environment"), text("provider"), text("status")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("credential_custody_transition_required", "invalid_param", "provider_account_last_active", "provider_accounts_ambiguous", "resource_not_found", "service_unavailable"), Handler: h(handlers.MerchantListPaymentProviders)},
	// or#288 routing dry run: which PSP a checkout would get, and why. The
	// answer is a projection of the PSP catalog, so it takes the same read.
	{Method: POST, Path: "/v1/merchant/payment-providers/routing/dry-run", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPaymentProvidersRead,
		Request: handlers.CheckoutRoutingDryRunRequest{}, Responses: []Reply{{200, handlers.CheckoutRoutingDryRunResponse{}}}, Errors: codes("catalog_scope_mismatch", "invalid_param"), Handler: h(handlers.MerchantDryRunCheckoutRouting)},
	{Method: GET, Path: "/v1/merchant/payment-providers/{provider}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPaymentProvidersRead,
		Query: params(text("environment")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("credential_custody_transition_required", "invalid_param", "payment_provider_not_found", "provider_account_last_active", "provider_accounts_ambiguous", "resource_not_found", "service_unavailable"), Handler: h(handlers.MerchantGetPaymentProvider)},
	// Metadata updates remain available with a read-only credential backend.
	// The service rejects write-only credentials when custody cannot retain them.
	{Method: PUT, Path: "/v1/merchant/payment-providers/{provider}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPaymentProvidersUpdate,
		Request: merchants.UpsertPaymentProviderConfigRequest{}, Responses: []Reply{{200, Untyped{}}}, Errors: codes("credential_custody_transition_required", "credential_operation_conflict", "credential_source_read_only", "credential_store_read_only", "invalid_param", "payment_provider_credentials_rejected", "provider_account_last_active", "provider_accounts_ambiguous", "psp_claim_requires_proof", "resource_not_found", "service_unavailable"), Handler: h(handlers.MerchantPutPaymentProvider)},
	// Lifecycle archives (#655/#656) write only the PSP row, never a secret,
	// never the provider: a terminated account is archivable from any
	// deployment.
	{Method: DELETE, Path: "/v1/merchant/payment-providers/{provider}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPaymentProvidersUpdate,
		Query: params(text("environment")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("credential_custody_transition_required", "invalid_param", "payment_provider_not_found", "provider_account_last_active", "provider_accounts_ambiguous", "resource_not_found", "service_unavailable"), Handler: h(handlers.MerchantDeletePaymentProvider)},
	{Method: POST, Path: "/v1/merchant/payment-providers/{provider}/accounts/{psp_id}/archive", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPaymentProvidersUpdate,
		Request: merchants.ArchivePaymentProviderAccountRequest{}, Responses: []Reply{{200, Untyped{}}}, Errors: codes("credential_custody_transition_required", "invalid_param", "provider_account_last_active", "provider_accounts_ambiguous", "resource_not_found", "service_unavailable"), Handler: h(handlers.MerchantArchivePaymentProviderAccount)},

	// The canonical callback surface. The configured provider identity
	// resolves its merchant in the runtime environment; runtime bindings and
	// signatures remain mandatory. {provider} is a rail, never a
	// merchant-specific PSP label. The body is the provider's own payload.
	{Method: POST, Path: "/v1/webhooks/{provider}/{account_id}", Group: Webhooks, Auth: AuthProvider, NoConn: true,
		Query: params(text("eventType")), Request: Untyped{}, Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "credential_custody_transition_required", "invalid_param", "resource_access_denied", "service_unavailable", "webhook_account_mismatch"), Handler: h(handlers.Webhook)},
}
