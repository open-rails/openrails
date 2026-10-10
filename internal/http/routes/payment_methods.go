package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// paymentMethodsRoutes is saved cards, with the agreements on them, for the
// customer and merchant staff, and the customer's default card per currency.
// A card is saved in one call; one the bank asks to authenticate is
// confirmed after.
var paymentMethodsRoutes = []Route{
	{Method: GET, Path: "/v1/admin/customers/{customer_id}/payment-methods", Group: Admin, Auth: AuthMerchant, Name: "ListPaymentMethods", Level: LevelRead,
		Query: params(cursorPage, idsParam), Responses: []Reply{{200, billing.ListPage[billing.PaymentMethod]{}}}, Errors: codes("invalid_cursor", "invalid_param"), Handler: h(handlers.ListCustomerPaymentMethods)},
	{Method: PUT, Path: "/v1/me/default-payment-methods/{currency}", Group: Customer, Auth: AuthCustomer,
		Request: billing.SetDefaultPaymentMethodParams{}, Responses: []Reply{{200, billing.DefaultPaymentMethod{}}}, Errors: codes("authentication_required", "card_declined", "default_payment_method_invalid", "invalid_param", "payment_method_not_psp_vaulted", "payment_method_psp_mismatch", "payment_method_same_vault", "payment_method_stale", "payment_provider_rejected", "rebill_terms_committed", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.SetDefaultPaymentMethod)},
	{Method: GET, Path: "/v1/me/payment-methods", Group: Customer, Auth: AuthCustomer,
		Query: cursorPage, Responses: []Reply{{200, billing.ListPage[billing.PaymentMethod]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListPaymentMethods)},
	{Method: POST, Path: "/v1/me/payment-methods", Group: Customer, Auth: AuthCustomer,
		Request: billing.CreatePaymentMethodParams{}, Responses: []Reply{{201, billing.PaymentMethod{}}}, Errors: codes("card_attempts_blocked", "card_declined", "card_not_saved", "card_requires_https", "invalid_param", "payment_duplicate_refused", "payment_method_stale", "payment_provider_rejected", "provider_outcome_unknown", "service_unavailable"), Handler: h(handlers.CreatePaymentMethod)},
	{Method: PUT, Path: "/v1/me/payment-methods/{id}", Group: Customer, Auth: AuthCustomer, IdempotencyKey: true,
		Request: billing.ReplacePaymentMethodCardParams{}, Responses: []Reply{{200, billing.PaymentMethod{}}, {202, nil}}, Errors: codes("card_declined", "card_not_saved", "card_requires_https", "invalid_param", "payment_method_update_failed", "payment_method_update_retry_required", "payment_method_update_unsupported", "payment_provider_rejected", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.ReplacePaymentMethodCard)},
	{Method: PATCH, Path: "/v1/me/payment-methods/{id}", Group: Customer, Auth: AuthCustomer,
		Request: billing.UpdatePaymentMethodParams{}, Responses: []Reply{{200, billing.PaymentMethod{}}}, Errors: codes("card_requires_https", "invalid_param", "payment_method_not_usable", "payment_method_update_unsupported", "payment_provider_rejected", "provider_outcome_unknown", "resource_not_found", "service_unavailable"), Handler: h(handlers.UpdatePaymentMethod)},
	{Method: POST, Path: "/v1/me/payment-methods/{id}/verify", Group: Customer, Auth: AuthCustomer, IdempotencyKey: true,
		Responses: []Reply{{200, billing.PaymentMethod{}}}, Errors: codes("card_attempts_blocked", "card_declined", "insufficient_funds", "invalid_param", "payment_method_not_usable", "payment_method_update_unsupported", "payment_provider_rejected", "provider_outcome_unknown", "resource_not_found", "service_unavailable"), Handler: h(handlers.VerifyPaymentMethod)},
	{Method: DELETE, Path: "/v1/me/payment-methods/{id}", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{202, nil}, {204, nil}}, Errors: codes("invalid_param", "payment_method_delete_failed", "payment_method_delete_unsupported", "rate_limit_exceeded", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.DeletePaymentMethod)},
	{Method: POST, Path: "/v1/me/payment-methods/{id}/confirm", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.PaymentMethod{}}}, Errors: codes("card_declined", "payment_provider_rejected", "resource_not_found", "service_unavailable"), Handler: h(handlers.ConfirmPaymentMethod)},
	{Method: POST, Path: "/v1/me/stripe/billing-portal-sessions", Group: Customer, Auth: AuthCustomer, When: FeatureStripePortal,
		Responses: []Reply{{200, handlers.StripePortalSession{}}}, Errors: codes("invalid_param", "resource_not_found", "route_not_found"), Handler: h(handlers.CreateStripePortalSession)},
}
