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
		Doc: "RecordExternalPayment records money the merchant received outside OpenRails for one order: part or " +
			"all of what a usage order owes, or another order's total. Recording the same TransactionID again " +
			"with the same terms answers the first payment; with other terms it is " +
			"billing.ErrIdempotencyKeyReused.",
		Request: billing.RecordExternalPaymentParams{}, Responses: []Reply{{200, billing.Payment{}}, {201, billing.Payment{}}}, Errors: codes("idempotency_key_reused", "invalid_param", "order_action_not_allowed", "order_has_recurring_line", "order_not_payable", "order_payment_in_progress", "payment_exceeds_due", "resource_not_found", "service_unavailable"), Handler: h(handlers.RecordExternalPayment)},
	{Method: GET, Path: "/v1/admin/payments", Group: Admin, Auth: AuthMerchant, Name: "ListPayments", Level: LevelRead,
		Doc:   "ListPayments is one page of the merchant's payments, newest first.",
		Query: billing.PaymentListParams{}, Responses: []Reply{{200, billing.ListPage[billing.Payment]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListPayments)},
	{Method: GET, Path: "/v1/admin/payments/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetPayment", Level: LevelRead,
		Args:      []any{billing.PaymentID{}},
		Doc:       "GetPayment reads one payment with its refunds.",
		Responses: []Reply{{200, billing.Payment{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.GetPayment)},
	{Method: POST, Path: "/v1/admin/payments/{id}/refunds", Group: Admin, Auth: AuthMerchant, Name: "RefundPayment", Level: LevelUpdate, IdempotencyKey: true,
		Args: []any{billing.PaymentID{}},
		Doc: "RefundPayment refunds a charge through its rail. The returned refund has Status \"succeeded\" once the " +
			"provider confirmed it, or \"pending\" while the durable refund operation is parked (provider write " +
			"gates) or reconciling an uncertain provider outcome; it settles without another call. A provider " +
			"refusal is a StatusError and leaves no refund recorded. idempotencyKey is required: a retry with it " +
			"returns the same refund and never refunds twice; the same key with other terms is " +
			"ErrIdempotencyKeyReused.",
		Request: billing.RefundPaymentParams{}, Responses: []Reply{{201, billing.Payment{}}, {202, billing.Payment{}}}, Errors: codes("idempotency_key_required", "idempotency_key_reused", "invalid_param", "payment_not_found", "payment_not_refundable", "provider_cancel_held", "rebill_terms_committed", "refund_failed", "refund_rail_unavailable", "refund_unsupported", "resource_conflict", "resource_not_found"), Handler: h(handlers.RefundPayment)},
	{Method: GET, Path: "/v1/admin/payment-attempts", Group: Admin, Auth: AuthMerchant, Name: "ListPaymentAttempts", Level: LevelRead,
		Doc:       "ListPaymentAttempts is one page of the merchant's payment attempts, newest first.",
		Query:     billing.PaymentAttemptListParams{},
		Responses: []Reply{{200, billing.ListPage[billing.PaymentAttempt]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListPaymentAttempts)},
	{Method: GET, Path: "/v1/admin/payment-attempts/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetPaymentAttempt", Level: LevelRead,
		Args:      []any{billing.PaymentAttemptID{}},
		Doc:       "GetPaymentAttempt reads one payment attempt.",
		Responses: []Reply{{200, billing.PaymentAttempt{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.GetPaymentAttempt)},
	{Method: GET, Path: "/v1/admin/renewals", Group: Admin, Auth: AuthMerchant, Name: "ListRenewals", Level: LevelRead,
		Doc:       "ListRenewals is one page of the merchant's renewals, latest due first.",
		Query:     billing.RenewalListParams{},
		Responses: []Reply{{200, billing.ListPage[billing.Renewal]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListRenewals)},
	{Method: GET, Path: "/v1/admin/renewals/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetRenewal", Level: LevelRead,
		Args:      []any{billing.RenewalID{}},
		Doc:       "GetRenewal reads one renewal with its attempts.",
		Responses: []Reply{{200, billing.Renewal{}}}, Errors: codes("invalid_param", "resource_not_found"), Handler: h(handlers.GetRenewal)},
	{Method: GET, Path: "/v1/me/payments", Group: Customer, Auth: AuthCustomer,
		Query: handlers.MyPaymentListQuery{}, Responses: []Reply{{200, billing.ListPage[billing.Payment]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListMyPayments)},
}
