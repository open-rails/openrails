package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

// ordersRoutes are purchases (#1168). The customer previews, buys in one
// call (create with a payment), or creates it with its embedded checkout on
// the checkout host (create with a checkout), pays, confirms after
// next_action, cancels and reads; paying needs the customer in person and an Idempotency-Key, and
// a failed payment answers 402 card_error with the order. The merchant's
// backend creates unpaid orders for a customer to pay (/v1/app/orders);
// staff inspect them, record external payments, waive usage debt or mark it
// uncollectible. Staff never initiate saved-card collection.
var (
	orderReadErrors   = codes("resource_not_found", "service_unavailable")
	orderLineErrors   = codes("already_owned", "invalid_param", "order_line_unavailable", "quantity_not_allowed", "resource_not_found", "service_unavailable")
	orderPayErrors    = codes("card_attempts_blocked", "card_declined", "customer_action_required", "entitlement_required", "idempotency_key_in_use", "idempotency_key_required", "idempotency_key_reused", "invalid_param", "order_not_payable", "order_payment_in_progress", "order_total_changed", "payment_failed", "payment_method_stale", "payment_option_unavailable", "payment_provider_rejected", "resource_conflict", "resource_not_found", "service_unavailable")
	orderCreateErrors = codes(append(append([]string{}, orderPayErrors...), "already_owned", "checkout_embed_origins_required", "customer_session_required", "hosted_checkout_unavailable", "order_line_unavailable", "quantity_not_allowed")...)
	usageActionErrors = codes("order_action_not_allowed", "resource_not_found", "service_unavailable")
)

var ordersRoutes = []Route{
	{Method: POST, Path: "/v1/me/orders/preview", Group: Customer, Auth: AuthCustomer,
		Request: billing.PreviewMyOrderParams{}, Responses: []Reply{{200, billing.OrderPreview{}}}, Errors: orderLineErrors, Handler: h(handlers.PreviewMyOrder)},
	{Method: POST, Path: "/v1/me/orders", Group: Customer, Auth: AuthCustomer, IdempotencyKey: true,
		Request: billing.CreateMyOrderParams{}, Responses: []Reply{{200, billing.Order{}}, {201, billing.Order{}}}, Errors: orderCreateErrors, Handler: h(handlers.CreateMyOrder)},
	{Method: GET, Path: "/v1/me/orders", Group: Customer, Auth: AuthCustomer,
		Query: params(cursorPage, idsParam, text("subscription_id"), text("status"), text("origin"), text("overdue")), Responses: []Reply{{200, billing.ListPage[billing.Order]{}}}, Errors: codes("invalid_cursor", "invalid_query", "service_unavailable"), Handler: h(handlers.ListMyOrders)},
	{Method: GET, Path: "/v1/me/orders/{id}", Group: Customer, Auth: AuthCustomer, Checkout: CheckoutOrder,
		Responses: []Reply{{200, billing.Order{}}}, Errors: orderReadErrors, Handler: h(handlers.GetMyOrder)},
	{Method: POST, Path: "/v1/me/orders/{id}/pay", Group: Customer, Auth: AuthCustomer, Checkout: CheckoutOrder, IdempotencyKey: true,
		Request: billing.PayOrderParams{}, Responses: []Reply{{200, billing.Order{}}}, Errors: orderPayErrors, Handler: h(handlers.PayMyOrder)},
	{Method: POST, Path: "/v1/me/orders/{id}/confirm", Group: Customer, Auth: AuthCustomer, Checkout: CheckoutOrder,
		Request: billing.ConfirmOrderParams{}, Responses: []Reply{{200, billing.Order{}}}, Errors: codes("card_declined", "customer_action_required", "customer_session_required", "invalid_param", "payment_failed", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.ConfirmMyOrder)},
	{Method: POST, Path: "/v1/me/orders/{id}/cancel", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.Order{}}}, Errors: codes("order_not_cancelable", "resource_not_found", "service_unavailable"), Handler: h(handlers.CancelMyOrder)},
	// A wallet's Solana Pay transaction request for an order awaiting it: the
	// label, then the transaction for the wallet's account. No credential:
	// a Solana Pay link is opened by the wallet, not the customer's session,
	// and the order names its merchant.
	{Method: GET, Path: "/v1/orders/{id}/solana-pay", Group: Checkout, Auth: AuthPublic, Merchant: MerchantOrder, When: FeatureSolana,
		Responses: []Reply{{200, handlers.SolanaPayGetResponse{}}}, Errors: codes("order_not_payable", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.GetOrderSolanaPay)},
	{Method: POST, Path: "/v1/orders/{id}/solana-pay", Group: Checkout, Auth: AuthPublic, Merchant: MerchantOrder, When: FeatureSolana,
		Request: handlers.SolanaPayPostRequest{}, Responses: []Reply{{200, handlers.SolanaPayPostResponse{}}}, Errors: codes("invalid_param", "order_not_payable", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.PostOrderSolanaPay)},
	{Method: POST, Path: "/v1/app/orders/preview", Group: App, Auth: AuthApplication, Permission: needOrders, Name: "PreviewOrder", Level: LevelRead,
		Request: billing.PreviewOrderParams{}, Responses: []Reply{{200, billing.OrderPreview{}}}, Errors: orderLineErrors, Handler: h(handlers.PreviewMerchantOrder)},
	{Method: POST, Path: "/v1/app/orders", Group: App, Auth: AuthApplication, Permission: needOrders, Name: "CreateOrder", IdempotencyKey: true,
		Request: billing.CreateOrderParams{}, Responses: []Reply{{200, billing.Order{}}, {201, billing.Order{}}},
		Errors: codes("already_owned", "checkout_embed_origins_required", "entitlement_required", "idempotency_key_in_use", "idempotency_key_required", "idempotency_key_reused", "invalid_param", "order_line_unavailable", "quantity_not_allowed", "resource_not_found", "service_unavailable"), Handler: h(handlers.CreateMerchantOrder)},
	{Method: POST, Path: "/v1/app/orders/{id}/cancel", Group: App, Auth: AuthApplication, Permission: needOrders, Name: "CancelOrder",
		Responses: []Reply{{200, billing.Order{}}}, Errors: codes("order_not_cancelable", "resource_not_found", "service_unavailable"), Handler: h(handlers.CancelMerchantOrder)},
	{Method: GET, Path: "/v1/admin/orders", Group: Admin, Auth: AuthMerchant, Name: "ListOrders", Level: LevelRead,
		Query: params(cursorPage, idsParam, text("customer_id"), text("price_id"), text("subscription_id"), text("status"), text("origin"), text("overdue")), Responses: []Reply{{200, billing.ListPage[billing.Order]{}}}, Errors: codes("invalid_cursor", "invalid_query", "service_unavailable"), Handler: h(handlers.ListOrders)},
	{Method: GET, Path: "/v1/admin/orders/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetOrder", Level: LevelRead,
		Responses: []Reply{{200, billing.Order{}}}, Errors: orderReadErrors, Handler: h(handlers.GetOrder)},
	{Method: POST, Path: "/v1/admin/orders/{id}/waive-balance", Group: Admin, Auth: AuthMerchant, Name: "WaiveOrderBalance", Level: LevelUpdate, Limit: middleware.AdminOperationDestructive,
		Responses: []Reply{{200, billing.Order{}}}, Errors: usageActionErrors, Handler: h(handlers.WaiveOrderBalance)},
	{Method: POST, Path: "/v1/admin/orders/{id}/mark-uncollectible", Group: Admin, Auth: AuthMerchant, Name: "MarkOrderUncollectible", Level: LevelUpdate, Limit: middleware.AdminOperationDestructive,
		Responses: []Reply{{200, billing.Order{}}}, Errors: usageActionErrors, Handler: h(handlers.MarkOrderUncollectible)},
}
