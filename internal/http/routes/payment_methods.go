package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/modules/checkout"
)

// paymentMethodsRoutes is saved payment methods, for the customer, a treasury
// co-manager and merchant staff, and which method pays what.
var paymentMethodsRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/payment-methods", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsRead,
		Responses: []Reply{{200, api.List[handlers.PaymentMethodResponse]{}}}, Errors: codes("invalid_param"), Handler: h(handlers.GetAdminUserPaymentMethods)},
	{Method: DELETE, Path: "/v1/merchant/customers/{customer_id}/payment-methods/{id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate, Limit: middleware.AdminOperationDestructive,
		Responses: []Reply{{202, nil}, {204, nil}}, Errors: codes("authentication_required", "invalid_param", "payment_method_delete_failed", "payment_method_delete_unsupported", "rate_limit_exceeded", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.AdminDeletePaymentMethod)},
	{Method: PUT, Path: "/v1/merchant/customers/{customer_id}/default-payment-method", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate,
		Request: Untyped{}, Responses: []Reply{{200, handlers.PaymentMethodResponse{}}}, Errors: codes("authentication_required", "invalid_param", "payment_method_not_usable", "resource_not_found"), Handler: h(handlers.AdminSetDefaultPaymentMethod)},
	{Method: PUT, Path: "/v1/me/collection-payment-method", Group: Customer, Auth: AuthCustomer, Scope: ScopeSubscriptionManagement,
		Request: handlers.CollectionPaymentMethodRequest{}, Responses: []Reply{{200, handlers.CollectionPaymentMethodResponse{}}}, Errors: codes("authentication_required", "invalid_param"), Handler: h(handlers.SetMyCollectionPaymentMethod)},
	{Method: PUT, Path: "/v1/me/default-payment-method", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Request: Untyped{}, Responses: []Reply{{200, handlers.PaymentMethodResponse{}}}, Errors: codes("authentication_required", "invalid_param", "payment_method_not_usable", "resource_not_found"), Handler: h(handlers.SetDefaultPaymentMethod)},
	{Method: GET, Path: "/v1/me/payment-methods", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: params(queryOf(handlers.ListPaymentMethodsQuery{}), integer("limit"), integer("offset")), Responses: []Reply{{200, api.List[handlers.PaymentMethodResponse]{}}}, Errors: codes("authentication_required", "invalid_param"), Handler: h(handlers.ListPaymentMethods)},
	{Method: POST, Path: "/v1/me/payment-methods/stripe-setup", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement, IdempotencyKey: true,
		Request: Untyped{}, Responses: []Reply{{200, checkout.StripeMethodSetupResponse{}}}, Errors: codes("card_attempts_blocked", "card_declined", "card_not_saved", "custodian_capture_unavailable", "customer_session_required", "idempotency_key_reused", "insufficient_funds", "invalid_param", "payment_method_required", "payment_method_stale", "payment_provider_rejected", "resource_access_denied", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.CreateStripeMethodSetup)},
	{Method: GET, Path: "/v1/me/payment-methods/stripe-setup/{id}", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{200, checkout.StripeMethodSetupResponse{}}}, Errors: codes("card_attempts_blocked", "card_declined", "card_not_saved", "custodian_capture_unavailable", "customer_session_required", "idempotency_key_reused", "insufficient_funds", "invalid_param", "payment_method_required", "payment_method_stale", "payment_provider_rejected", "resource_access_denied", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.GetStripeMethodSetup)},
	{Method: POST, Path: "/v1/me/payment-methods/stripe-setup/{id}/confirm", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{200, checkout.StripeMethodSetupResponse{}}}, Errors: codes("card_attempts_blocked", "card_declined", "card_not_saved", "custodian_capture_unavailable", "customer_session_required", "idempotency_key_reused", "insufficient_funds", "invalid_param", "payment_method_required", "payment_method_stale", "payment_provider_rejected", "resource_access_denied", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.ConfirmStripeMethodSetup)},
	{Method: POST, Path: "/v1/me/payment-methods", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Request: handlers.CreatePaymentMethodRequest{}, Responses: []Reply{{200, handlers.PaymentMethodResponse{}}}, Errors: codes("authentication_required", "card_attempts_blocked", "card_declined", "card_not_saved", "card_requires_https", "credential_custody_transition_required", "invalid_param", "payment_duplicate_refused", "payment_provider_rejected", "provider_outcome_unknown", "service_unavailable"), Handler: h(handlers.CreatePaymentMethod)},
	{Method: PUT, Path: "/v1/me/payment-methods/{id}", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement, IdempotencyKey: true,
		Request: handlers.UpdatePaymentMethodRequest{}, Responses: []Reply{{200, handlers.PaymentMethodResponse{}}, {202, nil}}, Errors: codes("authentication_required", "card_declined", "card_not_saved", "card_requires_https", "credential_custody_transition_required", "invalid_param", "payment_method_update_failed", "payment_method_update_retry_required", "payment_method_update_unsupported", "payment_provider_rejected", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.UpdatePaymentMethod)},
	{Method: DELETE, Path: "/v1/me/payment-methods/{id}", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{202, nil}, {204, nil}}, Errors: codes("authentication_required", "invalid_param", "payment_method_delete_failed", "payment_method_delete_unsupported", "rate_limit_exceeded", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.DeletePaymentMethod)},
	{Method: POST, Path: "/v1/me/billing-portal", Group: Customer, Auth: AuthCustomer, When: FeatureStripePortal,
		Responses: []Reply{{200, handlers.PortalResponse{}}}, Errors: codes("authentication_required", "invalid_param", "resource_not_found"), Handler: h(handlers.CreatePortalSession)},
	{Method: PUT, Path: "/v1/customers/{customer_id}/collection-payment-method", Group: Treasury, Auth: AuthCustomerGrant, Perm: billing.CustomerBillingUpdate,
		Request: handlers.CollectionPaymentMethodRequest{}, Responses: []Reply{{200, handlers.CollectionPaymentMethodResponse{}}}, Errors: codes("authentication_required", "invalid_param"), Handler: h(handlers.SetMyCollectionPaymentMethod)},
	{Method: GET, Path: "/v1/customers/{customer_id}/payment-methods", Group: Treasury, Auth: AuthCustomerGrant, Perm: billing.CustomerPaymentMethodsUpdate,
		Query: params(queryOf(handlers.ListPaymentMethodsQuery{}), integer("limit"), integer("offset")), Responses: []Reply{{200, api.List[handlers.PaymentMethodResponse]{}}}, Errors: codes("authentication_required", "invalid_param"), Handler: h(handlers.ListPaymentMethods)},
	{Method: POST, Path: "/v1/customers/{customer_id}/payment-methods", Group: Treasury, Auth: AuthCustomerGrant, Perm: billing.CustomerPaymentMethodsUpdate,
		Request: handlers.CreatePaymentMethodRequest{}, Responses: []Reply{{200, handlers.PaymentMethodResponse{}}}, Errors: codes("authentication_required", "card_attempts_blocked", "card_declined", "card_not_saved", "card_requires_https", "credential_custody_transition_required", "invalid_param", "payment_duplicate_refused", "payment_provider_rejected", "provider_outcome_unknown", "service_unavailable"), Handler: h(handlers.CreatePaymentMethod)},
	{Method: PUT, Path: "/v1/customers/{customer_id}/payment-methods/{id}", Group: Treasury, Auth: AuthCustomerGrant, Perm: billing.CustomerPaymentMethodsUpdate, IdempotencyKey: true,
		Request: handlers.UpdatePaymentMethodRequest{}, Responses: []Reply{{200, handlers.PaymentMethodResponse{}}, {202, nil}}, Errors: codes("authentication_required", "card_declined", "card_not_saved", "card_requires_https", "credential_custody_transition_required", "invalid_param", "payment_method_update_failed", "payment_method_update_retry_required", "payment_method_update_unsupported", "payment_provider_rejected", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.UpdatePaymentMethod)},
	{Method: DELETE, Path: "/v1/customers/{customer_id}/payment-methods/{id}", Group: Treasury, Auth: AuthCustomerGrant, Perm: billing.CustomerPaymentMethodsUpdate,
		Responses: []Reply{{202, nil}, {204, nil}}, Errors: codes("authentication_required", "invalid_param", "payment_method_delete_failed", "payment_method_delete_unsupported", "rate_limit_exceeded", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.DeletePaymentMethod)},
	{Method: POST, Path: "/v1/customers/{customer_id}/billing-portal", Group: Treasury, Auth: AuthCustomerGrant, Perm: billing.CustomerPaymentMethodsUpdate, When: FeatureStripePortal,
		Responses: []Reply{{200, handlers.PortalResponse{}}}, Errors: codes("authentication_required", "invalid_param", "resource_not_found"), Handler: h(handlers.CreatePortalSession)},
}
