package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/modules/checkout"
)

// cursorPage is the query of a cursor-paged list.
var cursorPage = params(text("cursor"), integer("limit"))

// paymentsRoutes is money that moved or tried to: payments and refunds,
// money the merchant received outside OpenRails, every authorization a PSP
// answered, rebill cycles, and the purchases a product archive left for
// review.
var paymentsRoutes = []Route{
	{Method: POST, Path: "/v1/admin/payments", Group: Admin, Auth: AuthMerchant, Name: "CreatePayment", Level: LevelWrite, Sensitive: true, Limit: middleware.AdminOperationOffChannel,
		Request: billing.CreatePaymentParams{}, Responses: []Reply{{200, billing.Payment{}}, {201, billing.Payment{}}}, Errors: codes("idempotency_key_reused", "invalid_param", "invoice_action_not_allowed", "order_has_recurring_line", "order_not_payable", "order_payment_in_progress", "payment_exceeds_due", "resource_not_found", "service_unavailable"), Handler: h(handlers.CreatePayment)},
	{Method: GET, Path: "/v1/admin/payments", Group: Admin, Auth: AuthMerchant, Name: "ListPayments", Level: LevelRead,
		Query: paymentListQuery(text("customer_id"), idsParam), Responses: []Reply{{200, billing.ListPage[billing.Payment]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListPayments)},
	{Method: GET, Path: "/v1/admin/payments/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetPayment", Level: LevelRead,
		Responses: []Reply{{200, billing.Payment{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.GetPayment)},
	{Method: POST, Path: "/v1/admin/payments/{id}/refunds", Group: Admin, Auth: AuthMerchant, Name: "RefundPayment", Level: LevelWrite, Sensitive: true, Limit: middleware.AdminOperationDestructive, IdempotencyKey: true,
		Request: billing.RefundPaymentParams{}, Responses: []Reply{{201, billing.Payment{}}, {202, billing.Payment{}}}, Errors: codes("idempotency_key_required", "idempotency_key_reused", "invalid_param", "payment_not_found", "payment_not_refundable", "provider_cancel_held", "rebill_terms_committed", "refund_failed", "refund_rail_unavailable", "refund_unsupported", "resource_conflict", "resource_not_found"), Handler: h(handlers.RefundPayment)},
	{Method: GET, Path: "/v1/admin/payment-attempts", Group: Admin, Auth: AuthMerchant, Name: "ListPaymentAttempts", Level: LevelRead,
		Query:     params(cursorPage, idsParam, text("avs_result"), text("card_entry"), text("category"), text("customer_id"), text("cvv_result"), text("cycle_id"), text("invoice_id"), text("kind"), text("observed_via"), text("order_id"), text("owner"), text("payment_id"), text("psp_id"), text("reason"), text("response_code"), text("since"), text("source"), text("subscription_id"), text("until")),
		Responses: []Reply{{200, billing.ListPage[billing.PaymentAttempt]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListPaymentAttempts)},
	{Method: GET, Path: "/v1/admin/payment-attempts/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetPaymentAttempt", Level: LevelRead,
		Responses: []Reply{{200, billing.PaymentAttempt{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.GetPaymentAttempt)},
	{Method: GET, Path: "/v1/admin/rebill-cycles", Group: Admin, Auth: AuthMerchant, Name: "ListRebillCycles", Level: LevelRead,
		Query:     params(cursorPage, idsParam, text("due_since"), text("due_until"), text("first_outcome"), text("miss_reason"), text("outcome"), text("owner"), text("psp_id"), text("subscription_id")),
		Responses: []Reply{{200, billing.ListPage[billing.RebillCycle]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListRebillCycles)},
	{Method: GET, Path: "/v1/admin/rebill-cycles/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetRebillCycle", Level: LevelRead,
		Responses: []Reply{{200, billing.RebillCycle{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.GetRebillCycle)},
	{Method: GET, Path: "/v1/me/payment-operations/{id}/authentication", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, checkout.StripeEngineAuthentication{}}}, Errors: paymentOperationErrors, Handler: h(handlers.GetStripePaymentAuthentication)},
	{Method: POST, Path: "/v1/me/payment-operations/{id}/authentication/confirm", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.PaymentOperation{}}}, Errors: paymentOperationErrors, Handler: h(handlers.ConfirmStripePaymentAuthentication)},
	{Method: GET, Path: "/v1/me/payments", Group: Customer, Auth: AuthCustomer,
		Query: paymentListQuery(), Responses: []Reply{{200, billing.ListPage[billing.Payment]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListMyPayments)},
}

var paymentOperationErrors = codes(append([]string{"payment_not_found"}, cardSetupErrors...)...)

// paymentListQuery is the query of a payment list.
func paymentListQuery(extra ...Param) []Param {
	return params(cursorPage, extra, text("invoice_id"), text("kind"), text("order_id"), text("rail"), text("status"), text("subscription_id"), text("transaction_id"))
}
