package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/cardguard"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/orders"
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

// answerOrder writes an order: 201 when created, 200 otherwise, and the
// replay header when a key's earlier request made it.
func answerOrder(r *httprequest.Request, order *billing.Order, created, replayed bool) {
	r.SetHeader("Cache-Control", "no-store")
	if replayed {
		r.SetHeader("Idempotent-Replayed", "true")
		r.JSON(http.StatusOK, order)
		return
	}
	if created {
		r.JSON(http.StatusCreated, order)
		return
	}
	r.JSON(http.StatusOK, order)
}

// PreviewMyOrder handles POST /v1/me/orders/preview.
func PreviewMyOrder(r *httprequest.Request) {
	var body billing.PreviewOrderParams
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
// is named.
func CreateMyOrder(r *httprequest.Request) {
	var body billing.CreateOrderParams
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
	order, replayed, err := svc.CreateOrder(r.Request.Context(), *actor, body, key)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	answerOrder(r, order, true, replayed)
}

// PayMyOrder handles POST /v1/me/orders/{id}/pay.
func PayMyOrder(r *httprequest.Request) {
	var body billing.PayOrderParams
	if !r.BindJSON(&body) {
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
	order, replayed, err := svc.PayOrder(r.Request.Context(), *actor, id, body, key)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	answerOrder(r, order, false, replayed)
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
	order, err := svc.ConfirmOrder(r.Request.Context(), *actor, id)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	answerOrder(r, order, false, false)
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
	answerOrder(r, order, false, false)
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
	answerOrder(r, order, false, false)
}

// ListMyOrders handles GET /v1/me/orders.
func ListMyOrders(r *httprequest.Request) {
	actor := orderActor(r, false)
	if actor == nil {
		return
	}
	page, ok := r.Page()
	svc := orderService(r)
	if !ok || svc == nil {
		return
	}
	out, err := svc.ListOrders(r.Request.Context(), *actor, billing.OrderListParams{PageRequest: page})
	if err != nil {
		writeOrderError(r, err)
		return
	}
	r.SetHeader("Cache-Control", "no-store")
	r.SuccessJSON(out)
}

// ListOrders handles GET /v1/admin/orders.
func ListOrders(r *httprequest.Request) {
	page, ok := r.Page()
	if !ok {
		return
	}
	params := billing.OrderListParams{PageRequest: page, Status: billing.OrderStatus(strings.TrimSpace(r.Query("status")))}
	switch params.Status {
	case "", billing.OrderOpen, billing.OrderRequiresAction, billing.OrderProcessing, billing.OrderPaid, billing.OrderCanceled, billing.OrderExpired:
	default:
		r.APIError(api.Coded(billing.CodeInvalidQuery, "status is invalid").WithParam("status"))
		return
	}
	for name, parse := range map[string]func(string) error{
		"customer_id": func(v string) (err error) { params.CustomerID, err = billing.ParseCustomerID(v); return },
		"price_id":    func(v string) (err error) { params.PriceID, err = billing.ParsePriceID(v); return },
	} {
		if raw := strings.TrimSpace(r.Query(name)); raw != "" && parse(raw) != nil {
			r.APIError(api.Coded(billing.CodeInvalidQuery, name+" is invalid").WithParam(name))
			return
		}
	}
	if params.IDs, ok = listIDs(r, billing.ParseOrderID); !ok {
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
	answerOrder(r, order, false, false)
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
	switch {
	case errors.As(err, &blocked):
		writeCardAttemptsBlocked(r, blocked.RetryAfter)
	case errors.Is(err, checkout.ErrPaymentMethodStale):
		r.ErrorCode(billing.CodePaymentMethodStale, "")
	case errors.Is(err, billing.ErrIdempotencyKeyReused):
		r.ErrorCode(billing.CodeIdempotencyKeyReused, "")
	case errors.Is(err, checkout.ErrCheckoutAttemptValidation):
		r.APIError(api.Coded(billing.CodeInvalidParam, err.Error()))
	default:
		writeRefusal(r, err, "order request failed")
	}
}

func lineParam(i int) string { return "lines[" + strconv.Itoa(i) + "]" }
