package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

// paymentsRoutes is money that moved or tried to: payments and refunds,
// money the merchant received outside OpenRails, every authorization a PSP
// answered and rebill cycles.
var paymentsRoutes = []Route{
	{Method: POST, Path: "/v1/admin/payments", Group: Admin, Auth: AuthMerchant, Name: "RecordExternalPayment", Level: LevelUpdate, Limit: middleware.AdminOperationOffChannel,
		Request: billing.RecordExternalPaymentParams{}, Responses: []Reply{{200, billing.Payment{}}, {201, billing.Payment{}}}, Errors: codes("idempotency_key_reused", "invalid_param", "order_action_not_allowed", "order_has_recurring_line", "order_not_payable", "order_payment_in_progress", "payment_exceeds_due", "resource_not_found", "service_unavailable"), Handler: h(handlers.RecordExternalPayment)},
	{Method: GET, Path: "/v1/admin/payments", Group: Admin, Auth: AuthMerchant, Name: "ListPayments", Level: LevelRead,
		Query: billing.PaymentListParams{}, Responses: []Reply{{200, billing.ListPage[billing.Payment]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListPayments)},
	{Method: GET, Path: "/v1/admin/payments/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetPayment", Level: LevelRead,
		Responses: []Reply{{200, billing.Payment{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.GetPayment)},
	{Method: POST, Path: "/v1/admin/payments/{id}/refunds", Group: Admin, Auth: AuthMerchant, Name: "RefundPayment", Level: LevelUpdate, IdempotencyKey: true,
		Request: billing.RefundPaymentParams{}, Responses: []Reply{{201, billing.Payment{}}, {202, billing.Payment{}}}, Errors: codes("idempotency_key_required", "idempotency_key_reused", "invalid_param", "payment_not_found", "payment_not_refundable", "provider_cancel_held", "rebill_terms_committed", "refund_failed", "refund_rail_unavailable", "refund_unsupported", "resource_conflict", "resource_not_found"), Handler: h(handlers.RefundPayment)},
	{Method: GET, Path: "/v1/admin/payment-attempts", Group: Admin, Auth: AuthMerchant, Name: "ListPaymentAttempts", Level: LevelRead,
		Query:     billing.PaymentAttemptListParams{},
		Responses: []Reply{{200, billing.ListPage[billing.PaymentAttempt]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListPaymentAttempts)},
	{Method: GET, Path: "/v1/admin/payment-attempts/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetPaymentAttempt", Level: LevelRead,
		Responses: []Reply{{200, billing.PaymentAttempt{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.GetPaymentAttempt)},
	{Method: GET, Path: "/v1/admin/renewals", Group: Admin, Auth: AuthMerchant, Name: "ListRenewals", Level: LevelRead,
		Query:     billing.RenewalListParams{},
		Responses: []Reply{{200, billing.ListPage[billing.Renewal]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListRenewals)},
	{Method: GET, Path: "/v1/admin/renewals/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetRenewal", Level: LevelRead,
		Responses: []Reply{{200, billing.Renewal{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.GetRenewal)},
	{Method: GET, Path: "/v1/me/payments", Group: Customer, Auth: AuthCustomer,
		Query: handlers.MyPaymentListQuery{}, Responses: []Reply{{200, billing.ListPage[billing.Payment]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListMyPayments)},
}
