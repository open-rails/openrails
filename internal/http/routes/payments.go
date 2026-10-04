package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/query"
)

// paymentsRoutes is money that moved or tried to: payments and refunds,
// every authorization a PSP answered, rebill cycles, and the purchases a
// product archive left for review.
var paymentsRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/payment-settlement-status", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPaymentsRead,
		Query: params(text("price_id")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ServicePaymentSettlementStatus)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/payments", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPaymentsRead,
		Query: params(integer("page"), integer("page_size")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("invalid_param"), Handler: h(handlers.GetAdminUserPayments)},
	{Method: POST, Path: "/v1/merchant/customers/{customer_id}/payments/off-channel", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantCustomerSettingsUpdate, Limit: middleware.AdminOperationOffChannel,
		Request: handlers.AdminOffChannelPaymentRequest{}, Responses: []Reply{{200, Untyped{}}, {201, Untyped{}}}, Errors: codes("catalog_scope_mismatch", "invalid_param"), Handler: h(handlers.AdminCreateOffChannelPayment)},
	{Method: GET, Path: "/v1/merchant/payments", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPaymentsRead,
		Query: queryOf(query.QueryOptions[payments.GetPaymentsFilters]{}), Responses: []Reply{{200, PathPage[api.PaymentObject]{}}}, Errors: codes("invalid_param"), Handler: h(handlers.GetAdminPayments)},
	{Method: GET, Path: "/v1/merchant/payments/{id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPaymentsRead,
		Responses: []Reply{{200, api.PaymentObject{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.GetAdminPayment)},
	{Method: POST, Path: "/v1/merchant/payments/{id}/refunds", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPaymentsRefund, Limit: middleware.AdminOperationDestructive, IdempotencyKey: true,
		Request: handlers.RefundRequest{}, Responses: []Reply{{200, api.PaymentObject{}}, {201, api.PaymentObject{}}, {202, api.PaymentObject{}}}, Errors: codes("invalid_param", "provider_cancel_held", "rebill_terms_committed", "resource_conflict"), Handler: h(handlers.AdminRefundPayment)},
	{Method: GET, Path: "/v1/merchant/payment-attempts", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPaymentsRead,
		Query: params(integer("limit"), integer("offset")), Responses: []Reply{{200, billing.Page[billing.PaymentAttempt]{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ListPaymentAttempts)},
	{Method: GET, Path: "/v1/merchant/payment-attempts/{id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPaymentsRead,
		Responses: []Reply{{200, billing.PaymentAttempt{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.GetPaymentAttempt)},
	{Method: GET, Path: "/v1/merchant/rebill-cycles", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPaymentsRead,
		Query: params(integer("limit"), integer("offset")), Responses: []Reply{{200, billing.Page[billing.RebillCycle]{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ListRebillCycles)},
	{Method: GET, Path: "/v1/merchant/rebill-cycles/{id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPaymentsRead,
		Responses: []Reply{{200, billing.RebillCycle{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.GetRebillCycle)},
	{Method: GET, Path: "/v1/merchant/purchase-reviews", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPaymentsRead,
		Query: params(integer("limit"), integer("offset"), text("product_archive_id"), text("status")), Responses: []Reply{{200, billing.Page[billing.PurchaseReview]{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ListPurchaseReviews)},
	{Method: POST, Path: "/v1/merchant/purchase-reviews/{id}/resolve", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPaymentsRefund, Limit: middleware.AdminOperationDestructive,
		Request: billing.ResolvePurchaseReviewParams{}, Responses: []Reply{{200, billing.PurchaseReview{}}}, Errors: codes("invalid_param", "provider_cancel_held", "resource_not_found"), Handler: h(handlers.ResolvePurchaseReview)},
	{Method: GET, Path: "/v1/me/payment-operations/{id}/authentication", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{200, checkout.StripeEngineAuthentication{}}}, Errors: codes("card_attempts_blocked", "card_declined", "card_not_saved", "custodian_capture_unavailable", "customer_session_required", "idempotency_key_reused", "insufficient_funds", "invalid_param", "payment_method_required", "payment_method_stale", "payment_not_found", "payment_provider_rejected", "resource_access_denied", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.GetStripePaymentAuthentication)},
	{Method: POST, Path: "/v1/me/payment-operations/{id}/authentication/confirm", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Responses: []Reply{{200, billing.PaymentOperation{}}}, Errors: codes("card_attempts_blocked", "card_declined", "card_not_saved", "custodian_capture_unavailable", "customer_session_required", "idempotency_key_reused", "insufficient_funds", "invalid_param", "payment_method_required", "payment_method_stale", "payment_not_found", "payment_provider_rejected", "resource_access_denied", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.ConfirmStripePaymentAuthentication)},
	{Method: GET, Path: "/v1/me/payments", Group: Customer, Auth: AuthCustomer, Scope: ScopeBillingManagement,
		Query: params(integer("limit"), integer("offset"), text("type")), Responses: []Reply{{200, PathPage[handlers.UserPaymentObject]{}}}, Errors: codes("authentication_required"), Handler: h(handlers.GetUserPayments)},
}
