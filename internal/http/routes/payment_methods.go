package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/modules/checkout"
)

var cardSetupErrors = codes("card_attempts_blocked", "card_declined", "card_not_saved", "custodian_capture_unavailable", "customer_session_required", "idempotency_key_reused", "insufficient_funds", "invalid_param", "payment_method_required", "payment_method_stale", "payment_provider_rejected", "resource_access_denied", "resource_conflict", "resource_not_found", "service_unavailable")

// paymentMethodsRoutes is saved cards, for the customer and merchant staff,
// and which card collects which currency's invoices.
var paymentMethodsRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/payment-methods", Group: Merchant, Auth: AuthMerchant, Name: "ListPaymentMethods", Level: LevelRead, Resources: res(ResCustomers),
		Query: cursorPage, Responses: []Reply{{200, billing.ListPage[billing.PaymentMethod]{}}}, Errors: codes("invalid_cursor", "invalid_param"), Handler: h(handlers.ListCustomerPaymentMethods)},
	{Method: DELETE, Path: "/v1/merchant/customers/{customer_id}/payment-methods/{id}", Group: Merchant, Auth: AuthMerchant, Name: "DeletePaymentMethod", Level: LevelWrite, Resources: res(ResCustomers), Sensitive: true, Limit: middleware.AdminOperationDestructive,
		Responses: []Reply{{202, nil}, {204, nil}}, Errors: codes("invalid_param", "payment_method_delete_failed", "payment_method_delete_unsupported", "rate_limit_exceeded", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.DeleteCustomerPaymentMethod)},
	{Method: PUT, Path: "/v1/me/collection-payment-method", Group: Customer, Auth: AuthCustomer, Scope: ScopeSubscriptionManagement,
		Request: billing.CollectionPaymentMethod{}, Responses: []Reply{{200, billing.CollectionPaymentMethod{}}}, Errors: codes("collection_payment_method_invalid", "invalid_param"), Handler: h(handlers.SetCollectionPaymentMethod)},
	{Method: GET, Path: "/v1/me/payment-methods", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: cursorPage, Responses: []Reply{{200, billing.ListPage[billing.PaymentMethod]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListPaymentMethods)},
	{Method: POST, Path: "/v1/me/payment-methods", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Request: billing.CreatePaymentMethodParams{}, Responses: []Reply{{201, billing.PaymentMethod{}}}, Errors: codes("card_attempts_blocked", "card_declined", "card_not_saved", "card_requires_https", "credential_custody_transition_required", "invalid_param", "payment_duplicate_refused", "payment_provider_rejected", "provider_outcome_unknown", "service_unavailable"), Handler: h(handlers.CreatePaymentMethod)},
	{Method: PUT, Path: "/v1/me/payment-methods/{id}", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement, IdempotencyKey: true,
		Request: billing.ReplacePaymentMethodCardParams{}, Responses: []Reply{{200, billing.PaymentMethod{}}, {202, nil}}, Errors: codes("card_declined", "card_not_saved", "card_requires_https", "credential_custody_transition_required", "invalid_param", "payment_method_update_failed", "payment_method_update_retry_required", "payment_method_update_unsupported", "payment_provider_rejected", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.ReplacePaymentMethodCard)},
	{Method: DELETE, Path: "/v1/me/payment-methods/{id}", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{202, nil}, {204, nil}}, Errors: codes("invalid_param", "payment_method_delete_failed", "payment_method_delete_unsupported", "rate_limit_exceeded", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.DeletePaymentMethod)},
	{Method: POST, Path: "/v1/me/payment-method-setups", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement, IdempotencyKey: true,
		Request: handlers.PaymentMethodSetupParams{}, Responses: []Reply{{200, checkout.PaymentMethodSetup{}}}, Errors: cardSetupErrors, Handler: h(handlers.CreatePaymentMethodSetup)},
	{Method: GET, Path: "/v1/me/payment-method-setups/{id}", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{200, checkout.PaymentMethodSetup{}}}, Errors: cardSetupErrors, Handler: h(handlers.GetPaymentMethodSetup)},
	{Method: POST, Path: "/v1/me/payment-method-setups/{id}/confirm", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{200, checkout.PaymentMethodSetup{}}}, Errors: cardSetupErrors, Handler: h(handlers.ConfirmPaymentMethodSetup)},
	{Method: POST, Path: "/v1/me/billing-portal", Group: Customer, Auth: AuthCustomer, When: FeatureStripePortal,
		Responses: []Reply{{200, handlers.PortalResponse{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.CreatePortalSession)},
}
