package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/hostedcheckout"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// Orders on /v1/me: the customer's own purchases. Paying, and every read of a
// payment's next action, is the customer in person.

// orderActor is the request's customer, nil after answering a refusal.
// present demands the customer in person, as paying does.
func orderActor(r *httprequest.Request, present bool) *billingservice.OrderActor {
	if present && !customerInitiatedChargeAllowed(r) {
		return nil
	}
	scope, ok := r.CustomerScope()
	if !ok {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return nil
	}
	return &billingservice.OrderActor{Customer: scope.Customer(), Principal: checkoutVerifiedPrincipal(r), ClientIP: r.ClientIP()}
}

// orderKey is the request's Idempotency-Key, "" after answering a refusal.
func orderKey(r *httprequest.Request) string {
	key := strings.TrimSpace(r.Request.Header.Get("Idempotency-Key"))
	switch {
	case key == "":
		r.ErrorCode("idempotency_key_required", "")
	case len(key) > 255:
		r.APIError(api.Coded(billing.CodeInvalidParam, "Idempotency-Key is 1 to 255 bytes").WithParam("Idempotency-Key"))
	case cardguard.ContainsPAN(key):
		r.APIError(api.Coded(billing.CodeInvalidParam, "Idempotency-Key must not contain card data").WithParam("Idempotency-Key"))
	default:
		return key
	}
	return ""
}

func orderService(r *httprequest.Request) *billingservice.Service {
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return nil
	}
	return svc
}

func orderID(r *httprequest.Request) (billing.OrderID, bool) {
	id, err := billing.ParseOrderID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeResourceNotFound, "order not found"))
		return billing.OrderID{}, false
	}
	return id, true
}

// answerOrder writes an order request's answer: the order (201 created, 200
// otherwise), or, when its payment failed, 402 card_error carrying the order,
// as Stripe's carries its PaymentIntent. A replay says so.
func answerOrder(r *httprequest.Request, answer billingservice.OrderAnswer) {
	r.SetHeader("Cache-Control", "no-store")
	if answer.Replayed {
		r.SetHeader("Idempotent-Replayed", "true")
	}
	if answer.Status != http.StatusPaymentRequired {
		r.JSON(answer.Status, answer.Order)
		return
	}
	out := api.Coded(answer.Code, "")
	if failure := answer.Order.Payment.LastPaymentError; failure != nil {
		out.Message = failure.Message
		out.Metadata = map[string]any{"failure": failure}
	}
	out.Order = answer.Order
	r.APIError(out)
}

// writeOrder writes an order read or changed without a payment.
func writeOrder(r *httprequest.Request, order *billing.Order) {
	r.SetHeader("Cache-Control", "no-store")
	r.JSON(http.StatusOK, order)
}

// PreviewMyOrder handles POST /v1/me/orders/preview.
func PreviewMyOrder(r *httprequest.Request) {
	var body billing.PreviewMyOrderParams
	if !r.BindJSON(&body) {
		return
	}
	actor := orderActor(r, false)
	svc := orderService(r)
	if actor == nil || svc == nil {
		return
	}
	preview, err := svc.PreviewOrder(r.Request.Context(), *actor, body)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	r.SuccessJSON(preview)
}

// CreateMyOrder handles POST /v1/me/orders: the one-call buy when a payment
// is named, an open order to pay later otherwise.
func CreateMyOrder(r *httprequest.Request) {
	var body billing.CreateMyOrderParams
	if !r.BindJSON(&body) {
		return
	}
	actor := orderActor(r, body.Payment != nil)
	if actor == nil {
		return
	}
	key := orderKey(r)
	svc := orderService(r)
	if key == "" || svc == nil {
		return
	}
	answer, err := svc.CreateOrder(r.Request.Context(), *actor, body, key)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	answerOrder(r, answer)
}

// PayMyOrder handles POST /v1/me/orders/{id}/pay.
func PayMyOrder(r *httprequest.Request) {
	var body billing.PayOrderParams
	if !r.BindJSON(&body) {
		return
	}
	if !checkoutPayment(r, body) {
		return
	}
	actor := orderActor(r, true)
	if actor == nil {
		return
	}
	id, ok := orderID(r)
	if !ok {
		return
	}
	key := orderKey(r)
	svc := orderService(r)
	if key == "" || svc == nil {
		return
	}
	answer, err := svc.PayOrder(r.Request.Context(), *actor, id, body, key)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	answerOrder(r, answer)
}

// ConfirmMyOrder handles POST /v1/me/orders/{id}/confirm: a nudge after the
// customer completed next_action; OpenRails reads the provider.
func ConfirmMyOrder(r *httprequest.Request) {
	actor := orderActor(r, true)
	if actor == nil {
		return
	}
	id, ok := orderID(r)
	svc := orderService(r)
	if !ok || svc == nil {
		return
	}
	answer, err := svc.ConfirmOrder(r.Request.Context(), *actor, id)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	answerOrder(r, answer)
}

// CancelMyOrder handles POST /v1/me/orders/{id}/cancel.
func CancelMyOrder(r *httprequest.Request) {
	actor := orderActor(r, false)
	if actor == nil {
		return
	}
	id, ok := orderID(r)
	svc := orderService(r)
	if !ok || svc == nil {
		return
	}
	order, err := svc.CancelOrder(r.Request.Context(), *actor, id)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	writeOrder(r, order)
}

// GetMyOrder handles GET /v1/me/orders/{id}: the resumable handle.
func GetMyOrder(r *httprequest.Request) {
	actor := orderActor(r, false)
	if actor == nil {
		return
	}
	id, ok := orderID(r)
	svc := orderService(r)
	if !ok || svc == nil {
		return
	}
	order, err := svc.GetOrder(r.Request.Context(), *actor, id)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	writeOrder(r, order)
}

// ListMyOrders handles GET /v1/me/orders.
func ListMyOrders(r *httprequest.Request) {
	actor := orderActor(r, false)
	if actor == nil {
		return
	}
	params, ok := orderListQuery(r, false)
	if !ok {
		return
	}
	svc := orderService(r)
	if svc == nil {
		return
	}
	out, err := svc.ListOrders(r.Request.Context(), *actor, params)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	r.SetHeader("Cache-Control", "no-store")
	r.SuccessJSON(out)
}

// ListOrders handles GET /v1/admin/orders.
func ListOrders(r *httprequest.Request) {
	params, ok := orderListQuery(r, true)
	if !ok {
		return
	}
	svc := orderService(r)
	if svc == nil {
		return
	}
	out, err := svc.ListMerchantOrders(r.Request.Context(), params)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	r.SetHeader("Cache-Control", "no-store")
	r.SuccessJSON(out)
}

// GetOrder handles GET /v1/admin/orders/{id}.
func GetOrder(r *httprequest.Request) {
	id, ok := orderID(r)
	svc := orderService(r)
	if !ok || svc == nil {
		return
	}
	order, err := svc.GetMerchantOrder(r.Request.Context(), id)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	writeOrder(r, order)
}

// writeOrderError answers an order refusal by its code.
func writeOrderError(r *httprequest.Request, err error) {
	var line *orders.LineError
	if errors.As(err, &line) {
		out := api.Coded(line.Code(), line.Refusal.Message).WithParam(lineParam(line.Index))
		out.Metadata = map[string]any{"code": line.Refusal.Code}
		if line.Refusal.OwnedBy != nil {
			out.Metadata["owned_by"] = *line.Refusal.OwnedBy
		}
		if line.Refusal.Hint != nil {
			out.Metadata["hint"] = *line.Refusal.Hint
		}
		r.APIError(out)
		return
	}
	var blocked *checkout.CardAttemptsBlockedError
	var refused *paymentmethods.PaymentMethodError
	switch {
	case errors.As(err, &blocked):
		writeCardAttemptsBlocked(r, blocked.RetryAfter)
	case errors.As(err, &refused):
		writePaymentMethodError(r, refused)
	case errors.Is(err, pgx.ErrNoRows):
		r.ErrorCode(billing.CodeResourceNotFound, "")
	case errors.Is(err, checkout.ErrPaymentMethodStale):
		writePaymentMethodStale(r)
	case errors.Is(err, billing.ErrIdempotencyKeyReused):
		r.ErrorCode(billing.CodeIdempotencyKeyReused, "")
	case errors.Is(err, checkout.ErrCheckoutAttemptValidation):
		r.APIError(api.Coded(billing.CodeInvalidParam, err.Error()))
	default:
		writeRefusal(r, err, "order request failed")
	}
}

func lineParam(i int) string { return "lines[" + strconv.Itoa(i) + "]" }

// PreviewMerchantOrder handles POST /v1/app/orders/preview: lines priced for
// one customer.
func PreviewMerchantOrder(r *httprequest.Request) {
	var body billing.PreviewOrderParams
	if !r.BindJSON(&body) {
		return
	}
	svc := orderService(r)
	if svc == nil {
		return
	}
	preview, err := svc.PreviewMerchantOrder(r.Request.Context(), body)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	r.SuccessJSON(preview)
}

// CreateMerchantOrder handles POST /v1/app/orders: an unpaid order for one
// customer, which they pay on the merchant's site.
func CreateMerchantOrder(r *httprequest.Request) {
	var body billing.CreateOrderParams
	if !r.BindJSON(&body) {
		return
	}
	svc := orderService(r)
	if svc == nil {
		return
	}
	order, replayed, err := svc.CreateMerchantOrder(r.Request.Context(), body, r.Header("Idempotency-Key"))
	if err != nil {
		writeOrderError(r, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	r.SetHeader("Cache-Control", "no-store")
	r.JSON(status, order)
}

// CancelMerchantOrder handles POST /v1/app/orders/{id}/cancel: withdraws an
// open order the merchant created.
func CancelMerchantOrder(r *httprequest.Request) {
	id, ok := orderID(r)
	svc := orderService(r)
	if !ok || svc == nil {
		return
	}
	order, err := svc.CancelMerchantOrder(r.Request.Context(), id)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	writeOrder(r, order)
}

// checkoutPayment refuses on the hosted checkout host what its secret does
// not open: a saved card the checkout does not offer (the merchant's opt-in,
// and cards the customer allowed to be shown again), or another attempt once
// the order took its most declines.
func checkoutPayment(r *httprequest.Request, body billing.PayOrderParams) bool {
	c, ok := hostedcheckout.FromContext(r.Request.Context())
	if !ok {
		return true
	}
	ctx := r.Request.Context()
	if pm := body.Payment.PaymentMethodID; pm != nil {
		offered := false
		if c.SavedPaymentMethods {
			methods, err := r.State.PaymentMethodService.ListByIDs(ctx, c.CustomerID.UUID(), []uuid.UUID{pm.UUID()})
			if err != nil {
				r.InternalError("failed to read the payment method", err)
				return false
			}
			views, err := paymentMethodsView(r, identity.CustomerID(c.CustomerID.UUID()), methods, false)
			if err != nil {
				r.InternalError("failed to read the payment method", err)
				return false
			}
			offered = len(hostedcheckout.SavedCards(views)) == 1
		}
		if !offered {
			r.APIError(api.Coded(billing.CodeResourceNotFound, "payment method not found").WithParam("payment.payment_method_id"))
			return false
		}
	}
	exhausted, err := hostedcheckout.NewStore(r.State.DB).Exhausted(ctx, c)
	if err != nil {
		r.InternalError("failed to count the checkout's attempts", err)
		return false
	}
	if exhausted {
		r.ErrorCode("checkout_attempts_exhausted", "")
		return false
	}
	return true
}

// orderListQuery reads an order list's filters; customer_id, price_id and
// ids are the merchant's.
func orderListQuery(r *httprequest.Request, merchant bool) (billing.OrderListParams, bool) {
	page, ok := r.Page()
	if !ok {
		return billing.OrderListParams{}, false
	}
	params := billing.OrderListParams{PageRequest: page, Status: billing.OrderStatus(strings.TrimSpace(r.Query("status"))), Origin: billing.OrderOrigin(strings.TrimSpace(r.Query("origin")))}
	switch params.Status {
	case "", billing.OrderOpen, billing.OrderProcessing, billing.OrderComplete, billing.OrderCanceled, billing.OrderExpired, billing.OrderVoid, billing.OrderUncollectible:
	default:
		r.APIError(api.Coded(billing.CodeInvalidQuery, "status is invalid").WithParam("status"))
		return params, false
	}
	switch params.Origin {
	case "", billing.OrderOriginCustomer, billing.OrderOriginMerchant, billing.OrderOriginSubscription, billing.OrderOriginUsage:
	default:
		r.APIError(api.Coded(billing.CodeInvalidQuery, "origin is invalid").WithParam("origin"))
		return params, false
	}
	switch strings.TrimSpace(r.Query("overdue")) {
	case "", "false":
	case "true":
		params.Overdue = true
	default:
		r.APIError(api.Coded(billing.CodeInvalidQuery, "overdue is true or false").WithParam("overdue"))
		return params, false
	}
	parsers := map[string]func(string) error{
		"subscription_id": func(v string) (err error) { params.SubscriptionID, err = billing.ParseSubscriptionID(v); return },
	}
	if merchant {
		parsers["customer_id"] = func(v string) (err error) { params.CustomerID, err = billing.ParseCustomerID(v); return }
		parsers["price_id"] = func(v string) (err error) { params.PriceID, err = billing.ParsePriceID(v); return }
	}
	for name, parse := range parsers {
		if raw := strings.TrimSpace(r.Query(name)); raw != "" && parse(raw) != nil {
			r.APIError(api.Coded(billing.CodeInvalidQuery, name+" is invalid").WithParam(name))
			return params, false
		}
	}
	if merchant {
		if params.IDs, ok = listIDs(r, billing.ParseOrderID); !ok {
			return params, false
		}
	}
	return params, true
}

// VoidOrder handles POST /v1/admin/orders/{id}/void: voids an unpaid usage
// order.
func VoidOrder(r *httprequest.Request) {
	usageOrderAction(r, (*billingservice.Service).VoidMerchantOrder)
}

// MarkOrderUncollectible handles POST /v1/admin/orders/{id}/mark-uncollectible:
// writes an open usage order off.
func MarkOrderUncollectible(r *httprequest.Request) {
	usageOrderAction(r, (*billingservice.Service).MarkMerchantOrderUncollectible)
}

func usageOrderAction(r *httprequest.Request, act func(*billingservice.Service, context.Context, billing.OrderID) (*billing.Order, error)) {
	id, ok := orderID(r)
	svc := orderService(r)
	if !ok || svc == nil {
		return
	}
	order, err := act(svc, r.Request.Context(), id)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	writeOrder(r, order)
}

// RetryOrderCollection handles POST /v1/admin/orders/{id}/retry-collection:
// charges what a usage order owes to one of its customer's cards. A failed
// charge answers 402 with the order.
func RetryOrderCollection(r *httprequest.Request) {
	id, ok := orderID(r)
	if !ok {
		return
	}
	key := orderKey(r)
	if key == "" {
		return
	}
	var body billing.RetryOrderCollectionParams
	if !r.BindJSON(&body) {
		return
	}
	svc := orderService(r)
	if svc == nil {
		return
	}
	body.IdempotencyKey = key
	answer, err := svc.RetryOrderCollection(r.Request.Context(), id, body)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	answerOrder(r, answer)
}
