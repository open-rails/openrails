package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// ordersRoutes are purchases (#1168). The customer previews, buys in one
// call (create with a payment), pays, confirms after next_action, cancels
// and reads; paying needs the customer in person and an Idempotency-Key, and
// a failed payment answers 402 card_error with the order. Staff read them
// and never pay.
var (
	orderReadErrors   = codes("resource_not_found", "service_unavailable")
	orderLineErrors   = codes("already_owned", "invalid_param", "order_line_unavailable", "quantity_not_allowed", "resource_not_found", "service_unavailable")
	orderPayErrors    = codes("card_attempts_blocked", "card_declined", "customer_action_required", "idempotency_key_in_use", "idempotency_key_required", "idempotency_key_reused", "invalid_param", "order_not_payable", "order_payment_in_progress", "order_total_changed", "payment_failed", "payment_method_stale", "payment_option_unavailable", "resource_conflict", "resource_not_found", "service_unavailable")
	orderCreateErrors = codes(append(append([]string{}, orderPayErrors...), "already_owned", "order_line_unavailable", "quantity_not_allowed")...)
)

var ordersRoutes = []Route{
	{Method: POST, Path: "/v1/me/orders/preview", Group: Customer, Auth: AuthCustomer,
		Request: billing.PreviewOrderParams{}, Responses: []Reply{{200, billing.OrderPreview{}}}, Errors: orderLineErrors, Handler: h(handlers.PreviewMyOrder)},
	{Method: POST, Path: "/v1/me/orders", Group: Customer, Auth: AuthCustomer, IdempotencyKey: true,
		Request: billing.CreateOrderParams{}, Responses: []Reply{{200, billing.Order{}}, {201, billing.Order{}}}, Errors: orderCreateErrors, Handler: h(handlers.CreateMyOrder)},
	{Method: GET, Path: "/v1/me/orders", Group: Customer, Auth: AuthCustomer,
		Query: cursorPage, Responses: []Reply{{200, billing.ListPage[billing.Order]{}}}, Errors: codes("invalid_cursor", "service_unavailable"), Handler: h(handlers.ListMyOrders)},
	{Method: GET, Path: "/v1/me/orders/{id}", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.Order{}}}, Errors: orderReadErrors, Handler: h(handlers.GetMyOrder)},
	{Method: POST, Path: "/v1/me/orders/{id}/pay", Group: Customer, Auth: AuthCustomer, IdempotencyKey: true,
		Request: billing.PayOrderParams{}, Responses: []Reply{{200, billing.Order{}}}, Errors: orderPayErrors, Handler: h(handlers.PayMyOrder)},
	{Method: POST, Path: "/v1/me/orders/{id}/confirm", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.Order{}}}, Errors: codes("card_declined", "customer_action_required", "customer_session_required", "payment_failed", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.ConfirmMyOrder)},
	{Method: POST, Path: "/v1/me/orders/{id}/cancel", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.Order{}}}, Errors: codes("order_not_cancelable", "resource_not_found", "service_unavailable"), Handler: h(handlers.CancelMyOrder)},
	{Method: GET, Path: "/v1/admin/orders", Group: Admin, Auth: AuthMerchant, Name: "ListOrders", Level: LevelRead,
		Query: params(cursorPage, idsParam, text("customer_id"), text("price_id"), text("status")), Responses: []Reply{{200, billing.ListPage[billing.Order]{}}}, Errors: codes("invalid_cursor", "invalid_query", "service_unavailable"), Handler: h(handlers.ListOrders)},
	{Method: GET, Path: "/v1/admin/orders/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetOrder", Level: LevelRead,
		Responses: []Reply{{200, billing.Order{}}}, Errors: orderReadErrors, Handler: h(handlers.GetOrder)},
}
