package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/paymentpage"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Orders: a customer previews, buys (one call, or create then pay), confirms
// after acting, cancels and reads their own orders. The order id is not a
// credential: every action is the customer's own, and another customer's
// order does not exist.

var (
	// ErrIdempotencyKeyInUse: the request first sent with the key is still
	// running.
	ErrIdempotencyKeyInUse = apperr.New(http.StatusConflict, billing.CodeIdempotencyKeyInUse, "The request first sent with this Idempotency-Key is still running.")
	errOrderKeyReused      = apperr.New(http.StatusUnprocessableEntity, billing.CodeIdempotencyKeyReused, "The Idempotency-Key was first sent with a different request.")
	errOrdersFenced        = apperr.New(http.StatusServiceUnavailable, billing.CodeServiceUnavailable, "The merchant takes no orders right now.")
)

// OrderActor is the customer acting on their orders, as the request proved
// them. Principal is the interactive session paying needs.
type OrderActor struct {
	Customer  billing.CustomerID
	Principal billingauth.Payer
	ClientIP  string
}

const (
	orderCreateOperation = "order_create"
	orderPayOperation    = "order_pay"
)

// orderKeyResult is what a key's request answered: the order, and the
// answer replayed verbatim (its next action read again, since no client
// secret is stored).
type orderKeyResult struct {
	OrderID uuid.UUID      `json:"order_id"`
	Digest  []byte         `json:"digest"`
	Status  int            `json:"status,omitempty"`
	Code    string         `json:"code,omitempty"`
	Order   *billing.Order `json:"order,omitempty"`
}

// OrderAnswer is an order request's answer: the order, and Status 201
// (created), 200, or 402 when its payment failed (Code card_declined or
// payment_failed; payment.last_payment_error says why). Replayed: the
// Idempotency-Key's first request gave this answer.
type OrderAnswer struct {
	Order    *billing.Order
	Status   int
	Code     string
	Replayed bool
}

// PreviewOrder prices lines for the customer without writing.
func (s *Service) PreviewOrder(ctx context.Context, actor OrderActor, params billing.PreviewMyOrderParams) (*billing.OrderPreview, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return nil, err
	}
	lines, err := orderLinesOf(ctx, rt, actor.Customer, params.Lines, params.Cart)
	if err != nil {
		return nil, err
	}
	quote, err := rt.Orders.Preview(ctx, actor.Customer.UUID(), lines)
	if err != nil {
		return nil, err
	}
	options, err := s.orderOptions(ctx, quote.Prices(), quote.Products())
	if err != nil {
		return nil, err
	}
	out := quote.Preview(options)
	return &out, nil
}

// orderPayment is a request's payment: a saved card, a card just entered or
// a Solana wallet.
type orderPayment struct {
	method uuid.UUID
	card   *payment.OrderCard
	solana *payment.OrderSolana
	hosted *payment.OrderHosted
}

func readOrderPayment(p *billing.OrderPaymentParams, reusable *bool) (orderPayment, error) {
	if p == nil {
		if reusable != nil {
			return orderPayment{}, apperr.Invalidf("reusable applies to a new card").WithParam("reusable")
		}
		return orderPayment{}, nil
	}
	saved, token, wallet, hosted := p.PaymentMethodID != nil && !p.PaymentMethodID.IsZero(), strings.TrimSpace(p.Token) != "", strings.TrimSpace(p.TokenSymbol) != "", strings.TrimSpace(p.ReturnURL) != ""
	switch {
	case btoi(saved)+btoi(token)+btoi(wallet)+btoi(hosted) != 1:
		return orderPayment{}, apperr.Invalidf("payment names exactly one of payment_method_id, token, token_symbol or return_url").WithParam("payment")
	case saved && (reusable != nil || p.PSPID != nil || p.BillingDetails != nil):
		return orderPayment{}, apperr.Invalidf("psp_id, billing_details and reusable describe a new card").WithParam("payment")
	case saved:
		return orderPayment{method: p.PaymentMethodID.UUID()}, nil
	case (wallet || hosted) && (reusable != nil || p.BillingDetails != nil):
		return orderPayment{}, apperr.Invalidf("billing_details and reusable describe a new card").WithParam("payment")
	case hosted:
		pay := &payment.OrderHosted{ReturnURL: strings.TrimSpace(p.ReturnURL)}
		if p.PSPID != nil {
			pay.PSPID = p.PSPID.UUID()
		}
		return orderPayment{hosted: pay}, nil
	case wallet:
		pay := &payment.OrderSolana{TokenSymbol: strings.TrimSpace(p.TokenSymbol)}
		if p.PSPID != nil {
			pay.PSPID = p.PSPID.UUID()
		}
		return orderPayment{solana: pay}, nil
	}
	card := &payment.OrderCard{Token: p.Token, Billing: p.BillingDetails, Reusable: reusable}
	if p.PSPID != nil {
		card.PSPID = p.PSPID.UUID()
	}
	return orderPayment{card: card}, nil
}

// CreateOrder answers the customer's open order of the lines, or freezes them
// into a new one, and, with a payment, pays it in the same call, or, with a
// pay URL, answers a fresh URL where the customer pays it. key, required
// with a payment, names its attempt: a later request with it answers what
// the first did.
func (s *Service) CreateOrder(ctx context.Context, actor OrderActor, params billing.CreateMyOrderParams, key string) (OrderAnswer, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return OrderAnswer{}, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return OrderAnswer{}, err
	}
	if params.PayURL == nil {
		return s.createOrder(ctx, rt, actor, params, key)
	}
	origin, err := s.payURLOrigin(ctx, params)
	if err != nil {
		return OrderAnswer{}, err
	}
	params.PayURL = nil // the pay URL is not part of the order or its key
	answer, err := s.createOrder(ctx, rt, actor, params, key)
	if err != nil {
		return OrderAnswer{}, err
	}
	return s.attachPayURL(ctx, rt, actor, answer, origin)
}

// createOrder is CreateOrder's order, under its key.
func (s *Service) createOrder(ctx context.Context, rt *orderRuntime, actor OrderActor, params billing.CreateMyOrderParams, key string) (OrderAnswer, error) {
	if err := checkOrderLines(params.Lines, params.Cart); err != nil {
		return OrderAnswer{}, err
	}
	pay, err := readOrderPayment(params.Payment, params.Reusable)
	if err != nil {
		return OrderAnswer{}, err
	}
	if params.Payment != nil && params.ExpectedTotal == nil {
		return OrderAnswer{}, apperr.Invalidf("expected_total is required with a payment").WithParam("expected_total")
	}
	if err := s.orderCustomerAllowed(ctx, actor.Customer); err != nil {
		return OrderAnswer{}, err
	}
	if params.Payment == nil {
		// The customer's open order of these lines is answered: asking again,
		// from any tab or reload, makes no second order.
		order, found, err := s.placeOrder(ctx, rt, actor, params)
		if err != nil {
			return OrderAnswer{}, err
		}
		status := http.StatusCreated
		if found {
			status = http.StatusOK
		}
		return s.orderAnswer(ctx, rt, actor, order.ID, status, payment.OrderCharge{}, false, nil)
	}
	// A one-call buy's key names its payment attempt.
	if strings.TrimSpace(key) == "" {
		return OrderAnswer{}, apperr.New(http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required with a payment")
	}
	digest, err := s.orderCreationDigest(ctx, params, params.Lines)
	if err != nil {
		return OrderAnswer{}, err
	}
	customer := actor.Customer.UUID()
	scoped := customer.String() + ":" + key
	claim, rec, err := rt.Idempotency.Begin(ctx, orderCreateOperation, scoped)
	if err != nil {
		return OrderAnswer{}, err
	}
	if claim == nil {
		return s.replayOrderAnswer(ctx, rt, actor, rec, digest, func(id uuid.UUID) (OrderAnswer, error) {
			charged, err := s.resumePayment(ctx, rt, actor, id, pay, "create:"+key)
			return s.orderAnswer(ctx, rt, actor, id, http.StatusOK, charged, true, err)
		})
	}
	work, stop := claim.Hold(ctx)
	work = db.WithCommitGuard(work, claim.InTx)
	id, replayed, charged, err := s.createClaimed(work, rt, actor, params, pay, key)
	stop() // before settling: a renewal never races the final state
	if errors.Is(context.Cause(work), idempotency.ErrClaimLost) || errors.Is(err, idempotency.ErrClaimLost) {
		return OrderAnswer{}, ErrIdempotencyKeyInUse
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	answer, err := s.orderAnswer(ctx, rt, actor, id, status, charged, replayed, err)
	s.settleOrderClaim(ctx, claim, id, digest, answer, err)
	return answer, err
}

// createClaimed runs a one-call buy under its key's claim: the customer's
// open order of the lines (or a new one), paid under the key's attempt.
func (s *Service) createClaimed(ctx context.Context, rt *orderRuntime, actor OrderActor, params billing.CreateMyOrderParams, pay orderPayment, key string) (uuid.UUID, bool, payment.OrderCharge, error) {
	customer := actor.Customer.UUID()
	if params.Payment != nil {
		// A payment that cannot be made refuses the order before it exists.
		lines, err := orderLinesOf(ctx, rt, actor.Customer, params.Lines, params.Cart)
		if err != nil {
			return uuid.Nil, false, payment.OrderCharge{}, err
		}
		quote, err := rt.Orders.Preview(ctx, customer, lines)
		if err == nil {
			err = s.orderPaymentUsable(ctx, quote, pay, actor.Customer)
		}
		if err != nil {
			return uuid.Nil, false, payment.OrderCharge{}, err
		}
	}
	order, found, err := s.placeOrder(ctx, rt, actor, params)
	if err != nil {
		return uuid.Nil, false, payment.OrderCharge{}, err
	}
	var charged payment.OrderCharge
	if order.Status == string(billing.OrderOpen) {
		charged, err = s.payOrder(ctx, rt, actor, order, pay, "create:"+key)
	}
	return order.ID, found, charged, err
}

// placeOrder answers the customer's open order of params' lines (found), or
// creates it. A cart's order is always a new one: buying the cart empties it.
func (s *Service) placeOrder(ctx context.Context, rt *orderRuntime, actor OrderActor, params billing.CreateMyOrderParams) (*orders.Order, bool, error) {
	in := orders.CreateInput{CustomerID: actor.Customer.UUID(), Origin: billing.OrderOriginCustomer, Lines: params.Lines, FromCart: params.Cart,
		ExpectedTotal: params.ExpectedTotal, TTL: orders.CustomerTTL}
	if !params.Cart {
		digest, err := s.orderLinesDigest(ctx, params.Lines)
		if err != nil {
			return nil, false, err
		}
		if in.LinesDigest = digest; digest != nil {
			return rt.Orders.FindOrCreate(ctx, in)
		}
	}
	order, err := rt.Orders.Create(ctx, in)
	return order, false, err
}

// resumePayment runs a one-call buy's payment again under its key: the same
// attempt, which never charges twice.
func (s *Service) resumePayment(ctx context.Context, rt *orderRuntime, actor OrderActor, id uuid.UUID, pay orderPayment, key string) (payment.OrderCharge, error) {
	order, err := rt.Orders.GetForCustomer(ctx, actor.Customer.UUID(), id)
	if err != nil || (pay.method == uuid.Nil && pay.card == nil && pay.solana == nil && pay.hosted == nil) || order.Status != string(billing.OrderOpen) {
		return payment.OrderCharge{}, err
	}
	return s.payOrder(ctx, rt, actor, order, pay, key)
}

// orderAnswer reads the order a request acted on into its answer: 402 when
// its payment failed. err passes through.
func (s *Service) orderAnswer(ctx context.Context, rt *orderRuntime, actor OrderActor, id uuid.UUID, status int, charged payment.OrderCharge, replayed bool, err error) (OrderAnswer, error) {
	if err != nil {
		return OrderAnswer{}, err
	}
	view, err := s.orderView(ctx, rt, actor, id)
	if err != nil {
		return OrderAnswer{}, err
	}
	out := OrderAnswer{Order: view, Status: status, Replayed: replayed}
	if charged.Failed && view.Payment.Status == billing.OrderPaymentRequiresPaymentMethod {
		out.Status, out.Code = http.StatusPaymentRequired, billing.CodePaymentFailed
		if charged.Declined {
			out.Code = billing.CodeCardDeclined
		}
	}
	return out, nil
}

// replayOrderAnswer answers a key whose request finished: the answer it gave,
// or, for a record without one, legacy(order).
func (s *Service) replayOrderAnswer(ctx context.Context, rt *orderRuntime, actor OrderActor, rec *idempotency.Record, digest []byte, legacy func(uuid.UUID) (OrderAnswer, error)) (OrderAnswer, error) {
	if rec.Status != idempotency.StatusSucceeded {
		return OrderAnswer{}, ErrIdempotencyKeyInUse
	}
	var result orderKeyResult
	if err := json.Unmarshal(rec.Result, &result); err != nil {
		return OrderAnswer{}, err
	}
	if string(result.Digest) != string(digest) {
		return OrderAnswer{}, errOrderKeyReused
	}
	if result.Order == nil {
		return legacy(result.OrderID)
	}
	if result.Order.Payment.Status == billing.OrderPaymentRequiresAction {
		order, err := rt.Orders.GetForCustomer(ctx, actor.Customer.UUID(), result.OrderID)
		if err != nil {
			return OrderAnswer{}, err
		}
		resolver, _ := s.rt.CollectionResolver.(intents.StripeEngineServiceResolver)
		if result.Order.Payment.NextAction, err = rt.Attempts.OrderNextAction(ctx, order, actor.Principal, resolver); err != nil {
			return OrderAnswer{}, err
		}
	}
	return OrderAnswer{Order: result.Order, Status: result.Status, Code: result.Code, Replayed: true}, nil
}

// settleOrderClaim records a key's answer, its next action left out.
func (s *Service) settleOrderClaim(ctx context.Context, claim *idempotency.Claim, order uuid.UUID, digest []byte, answer OrderAnswer, err error) {
	if err != nil {
		_ = claim.Fail(ctx, err)
		return
	}
	stored := *answer.Order
	stored.Payment.NextAction = nil
	raw, _ := json.Marshal(orderKeyResult{OrderID: order, Digest: digest, Status: answer.Status, Code: answer.Code, Order: &stored})
	_ = claim.Complete(ctx, raw)
}

// PayOrder pays the customer's open order. One key is one attempt.
func (s *Service) PayOrder(ctx context.Context, actor OrderActor, id billing.OrderID, params billing.PayOrderParams, key string) (OrderAnswer, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return OrderAnswer{}, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return OrderAnswer{}, err
	}
	pay, err := readOrderPayment(&params.Payment, params.Reusable)
	if err != nil {
		return OrderAnswer{}, err
	}
	order, err := rt.Orders.GetForCustomer(ctx, actor.Customer.UUID(), id.UUID())
	if err != nil {
		return OrderAnswer{}, err
	}
	if order.CollectionOwner != string(billing.OrderCollectionEngine) {
		return OrderAnswer{}, orders.ErrNotPayable
	}
	// A usage order's collection is a durable operation that waits out
	// disabled writes and provider recovery itself.
	if order.Origin != string(billing.OrderOriginUsage) {
		if err := s.orderCustomerAllowed(ctx, actor.Customer); err != nil {
			return OrderAnswer{}, err
		}
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return OrderAnswer{}, err
	}
	digest := orderDigest(params)
	scoped := order.ID.String() + ":" + key
	claim, rec, err := rt.Idempotency.Begin(ctx, orderPayOperation, scoped)
	if err != nil {
		return OrderAnswer{}, err
	}
	if claim == nil {
		return s.replayOrderAnswer(ctx, rt, actor, rec, digest, func(id uuid.UUID) (OrderAnswer, error) {
			return s.orderAnswer(ctx, rt, actor, id, http.StatusOK, payment.OrderCharge{}, true, nil)
		})
	}
	work, stop := claim.Hold(ctx)
	work = db.WithCommitGuard(work, claim.InTx)
	var charged payment.OrderCharge
	var replayed bool
	switch {
	case params.ExpectedTotal != order.Total:
		err = orders.ErrTotalChanged
	case order.Origin == string(billing.OrderOriginSubscription):
		charged, replayed, err = s.payRenewal(work, actor, order, pay, key)
	case order.Origin == string(billing.OrderOriginUsage):
		charged, replayed, err = s.payUsage(work, actor, order, pay, key)
	default:
		_, attemptErr := rt.DB.Gen(work).GetOrderAttempt(work, gen.GetOrderAttemptParams{MerchantID: mid.UUID(), ID: payment.OrderAttemptID(mid.UUID(), order.ID, "pay:"+key)})
		replayed = attemptErr == nil
		charged, err = s.payOrder(work, rt, actor, order, pay, "pay:"+key)
	}
	stop() // before settling: a renewal never races the final state
	if errors.Is(context.Cause(work), idempotency.ErrClaimLost) || errors.Is(err, idempotency.ErrClaimLost) {
		return OrderAnswer{}, ErrIdempotencyKeyInUse
	}
	answer, err := s.orderAnswer(ctx, rt, actor, order.ID, http.StatusOK, charged, replayed, err)
	s.settleOrderClaim(ctx, claim, order.ID, digest, answer, err)
	return answer, err
}

// payOrder pays a purchase. A saved card awaiting the customer's consent
// (its brand changed) is verified with them present first.
func (s *Service) payOrder(ctx context.Context, rt *orderRuntime, actor OrderActor, order *orders.Order, pay orderPayment, key string) (payment.OrderCharge, error) {
	if order.CollectionOwner != string(billing.OrderCollectionEngine) {
		return payment.OrderCharge{}, orders.ErrNotPayable
	}
	quote, err := s.frozenQuote(ctx, order)
	if err != nil {
		return payment.OrderCharge{}, err
	}
	if err := s.reconsentPaying(ctx, actor.Customer.UUID(), pay.method, key); err != nil {
		return payment.OrderCharge{}, err
	}
	user := &payment.UserIdentity{ID: actor.Customer.String(), ClientIP: actor.ClientIP}
	return rt.Attempts.PayOrder(ctx, payment.OrderPayInput{Order: order, PaymentMethodID: pay.method, Card: pay.card, Solana: pay.solana, Hosted: pay.hosted, Key: key, User: user, Prices: quote.prices, Products: quote.products})
}

// ConfirmOrder reads the provider after the customer completed next_action;
// an attempt that failed answers 402.
func (s *Service) ConfirmOrder(ctx context.Context, actor OrderActor, id billing.OrderID, params billing.ConfirmOrderParams) (OrderAnswer, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return OrderAnswer{}, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return OrderAnswer{}, err
	}
	order, err := rt.Orders.GetForCustomer(ctx, actor.Customer.UUID(), id.UUID())
	if err != nil {
		return OrderAnswer{}, err
	}
	var signature string
	if params.Signature != nil {
		signature = strings.TrimSpace(*params.Signature)
	}
	charged, err := rt.Attempts.ConfirmOrder(ctx, order, actor.Principal, signature)
	return s.orderAnswer(ctx, rt, actor, order.ID, http.StatusOK, charged, false, err)
}

// CancelOrder cancels the customer's open order.
func (s *Service) CancelOrder(ctx context.Context, actor OrderActor, id billing.OrderID) (*billing.Order, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return nil, err
	}
	if _, err := rt.Orders.Cancel(ctx, actor.Customer.UUID(), id.UUID()); err != nil {
		return nil, err
	}
	return s.orderView(ctx, rt, actor, id.UUID())
}

// GetOrder reads the customer's own order.
func (s *Service) GetOrder(ctx context.Context, actor OrderActor, id billing.OrderID) (*billing.Order, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return nil, err
	}
	return s.orderView(ctx, rt, actor, id.UUID())
}

// ListOrders pages the customer's own orders, newest first. A list carries
// no next_action or payment options; read the order for them.
func (s *Service) ListOrders(ctx context.Context, actor OrderActor, params billing.OrderListParams) (billing.ListPage[billing.Order], error) {
	if actor.Customer.IsZero() {
		return billing.ListPage[billing.Order]{}, orders.ErrNotFound
	}
	params.CustomerID = actor.Customer
	return s.listOrders(ctx, params)
}

// ListMerchantOrders pages the merchant's orders for staff, newest first.
func (s *Service) ListMerchantOrders(ctx context.Context, params billing.OrderListParams) (billing.ListPage[billing.Order], error) {
	return s.listOrders(ctx, params)
}

// GetMerchantOrder reads one order for staff. Staff never pay, so it carries
// no next_action.
func (s *Service) GetMerchantOrder(ctx context.Context, id billing.OrderID) (*billing.Order, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return nil, err
	}
	order, err := rt.Orders.Get(ctx, id.UUID())
	if err != nil {
		return nil, err
	}
	view := order.View(nil, nil)
	return &view, s.markDelinquent(ctx, &view)
}

func (s *Service) listOrders(ctx context.Context, params billing.OrderListParams) (billing.ListPage[billing.Order], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return billing.ListPage[billing.Order]{}, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return billing.ListPage[billing.Order]{}, err
	}
	if params.IDs != nil {
		ids := make([]uuid.UUID, len(params.IDs))
		for i, id := range params.IDs {
			ids[i] = id.UUID()
		}
		rows, err := rt.Orders.ListByIDs(ctx, ids)
		if err != nil {
			return billing.ListPage[billing.Order]{}, err
		}
		out := billing.ListPage[billing.Order]{Items: make([]billing.Order, 0, len(rows))}
		for _, o := range rows {
			if params.CustomerID.IsZero() || o.CustomerID == params.CustomerID.UUID() {
				out.Items = append(out.Items, o.View(nil, nil))
			}
		}
		return out, s.markDelinquentPage(ctx, out.Items)
	}
	var filter orders.ListFilter
	if !params.CustomerID.IsZero() {
		filter.CustomerID = new(params.CustomerID.UUID())
	}
	if !params.PriceID.IsZero() {
		price, err := s.rt.PriceService.GetByPublicID(ctx, params.PriceID)
		if db.IsNotFound(err) {
			return billing.ListPage[billing.Order]{Items: []billing.Order{}}, nil
		}
		if err != nil {
			return billing.ListPage[billing.Order]{}, err
		}
		filter.PriceID = &price.ID
	}
	if params.Status != "" {
		filter.Status = new(string(params.Status))
	}
	if params.Origin != "" {
		filter.Origin = new(string(params.Origin))
	}
	if !params.SubscriptionID.IsZero() {
		filter.SubscriptionID = new(params.SubscriptionID.UUID())
	}
	if params.Overdue {
		filter.OverdueAt = new(s.now())
	}
	page, err := rt.Orders.List(ctx, filter, params.PageRequest)
	if err != nil {
		return billing.ListPage[billing.Order]{}, err
	}
	out := billing.ListPage[billing.Order]{Items: make([]billing.Order, len(page.Items)), Next: page.Next}
	for i, o := range page.Items {
		out.Items[i] = o.View(nil, nil)
	}
	return out, s.markDelinquentPage(ctx, out.Items)
}

// orderView is the order as its customer reads it, with the next action of a
// payment awaiting them and the options of an open order.
func (s *Service) orderView(ctx context.Context, rt *orderRuntime, actor OrderActor, id uuid.UUID) (*billing.Order, error) {
	order, err := rt.Orders.GetForCustomer(ctx, actor.Customer.UUID(), id)
	if err != nil {
		return nil, err
	}
	var next *billing.NextAction
	if order.CollectionOwner == string(billing.OrderCollectionEngine) && order.AwaitsCustomer() {
		resolver, _ := s.rt.CollectionResolver.(intents.StripeEngineServiceResolver)
		if next, err = rt.Attempts.OrderNextAction(ctx, order, actor.Principal, resolver); err != nil {
			return nil, err
		}
	}
	options, err := s.openOrderOptions(ctx, order)
	if err != nil {
		return nil, err
	}
	view := order.View(next, options)
	return &view, s.markDelinquent(ctx, &view)
}

// openOrderOptions are what can pay an open order; none for any other.
func (s *Service) openOrderOptions(ctx context.Context, order *orders.Order) ([]billing.OrderPaymentOption, error) {
	switch {
	case order.CollectionOwner != string(billing.OrderCollectionEngine):
		return nil, nil
	case order.Status != string(billing.OrderOpen):
		return nil, nil
	case order.Origin == string(billing.OrderOriginSubscription):
		return s.renewalOptions(ctx, order)
	case order.Origin == string(billing.OrderOriginUsage):
		return s.usageOptions(ctx, order)
	}
	quote, err := s.frozenQuote(ctx, order)
	if err != nil {
		return nil, err
	}
	return s.orderOptions(ctx, quote.prices, quote.products)
}

type frozenLines struct {
	prices   []*models.Price
	products []*models.Product
}

// frozenQuote reads the catalog rows an order's lines name, for routing.
func (s *Service) frozenQuote(ctx context.Context, order *orders.Order) (frozenLines, error) {
	var out frozenLines
	for _, l := range order.Lines {
		if l.PriceID == nil || l.ProductID == nil {
			return out, orders.ErrNotPayable
		}
		price, err := s.rt.PriceService.GetByID(ctx, *l.PriceID)
		if err != nil {
			return out, err
		}
		product, err := s.rt.ProductService.GetByID(ctx, *l.ProductID)
		if err != nil {
			return out, err
		}
		out.prices, out.products = append(out.prices, price), append(out.products, product)
	}
	return out, nil
}

func (s *Service) orderOptions(ctx context.Context, prices []*models.Price, products []*models.Product) ([]billing.OrderPaymentOption, error) {
	options, err := s.rt.OrderAttemptService.OrderOptions(ctx, prices, products)
	if err != nil {
		return nil, err
	}
	out := make([]billing.OrderPaymentOption, 0, len(options))
	for _, o := range options {
		out = append(out, billing.OrderPaymentOption{PSPID: billing.PSPID(o.PSPID), Rail: o.Rail, Accepts: o.Accepts()})
	}
	return out, nil
}

// orderPaymentUsable refuses, before an order exists, a saved card no PSP
// that sells its lines can charge, or a new card's PSP that takes none.
func (s *Service) orderPaymentUsable(ctx context.Context, quote *orders.Quote, pay orderPayment, customer billing.CustomerID) error {
	if i := quote.Refused(); i >= 0 {
		return &orders.LineError{Index: i, Refusal: *quote.Lines[i].Refusal}
	}
	options, err := s.rt.OrderAttemptService.OrderOptions(ctx, quote.Prices(), quote.Products())
	if err != nil {
		return err
	}
	if pay.card != nil {
		_, err := payment.NewCardOption(options, pay.card.PSPID)
		return err
	}
	if pay.solana != nil {
		_, err := payment.SolanaOption(options, pay.solana.PSPID)
		return err
	}
	if pay.hosted != nil {
		if err := s.rt.OrderAttemptService.CheckReturnURL(pay.hosted.ReturnURL); err != nil {
			return err
		}
		_, err := payment.HostedOption(options, pay.hosted.PSPID)
		return err
	}
	pm, err := s.rt.PaymentMethodService.ValidatePaymentMethodOperation(ctx, pay.method, customer.String())
	if err != nil {
		return fmt.Errorf("%w: %w", payment.ErrPaymentMethodStale, err)
	}
	if pm.Status != paymentmethods.StatusActive {
		return fmt.Errorf("%w: the card is %s", payment.ErrPaymentMethodStale, pm.Status)
	}
	for _, o := range options {
		if strings.EqualFold(string(pm.Rail), o.Rail) && pm.ChargeableOn(o.PSPID) {
			return nil
		}
	}
	return orders.ErrOptionMissing
}

// orderCustomerAllowed refuses every customer of a merchant whose provider
// writes are fenced.
func (s *Service) orderCustomerAllowed(ctx context.Context, _ billing.CustomerID) error {
	if err := s.rt.OrderAttemptService.TakesOrders(ctx); err != nil {
		return errOrdersFenced
	}
	return nil
}

type orderRuntime struct {
	Orders      *orders.Service
	Attempts    *payment.OrderAttemptService
	Idempotency *idempotency.Store
	DB          interface {
		Gen(context.Context) *gen.Queries
	}
}

func (s *Service) orderRuntime() (*orderRuntime, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.Orders == nil || rt.OrderAttemptService == nil || rt.Idempotency == nil || rt.DB == nil || rt.PriceService == nil || rt.ProductService == nil || rt.PaymentMethodService == nil {
		return nil, errors.New("billing service: orders unavailable")
	}
	return &orderRuntime{Orders: rt.Orders, Attempts: rt.OrderAttemptService, Idempotency: rt.Idempotency, DB: rt.DB}, nil
}

func orderDigest(v any) []byte {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return sum[:]
}

// RecordPayment records money the merchant received outside OpenRails for an
// order: part or all of what a usage order owes, or another order's total.
// created is false when the same remittance was already recorded.
func (s *Service) RecordPayment(ctx context.Context, params billing.RecordExternalPaymentParams) (payment *models.Payment, created bool, err error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return nil, false, err
	}
	order, err := rt.Orders.Get(ctx, params.OrderID.UUID())
	if err != nil {
		return nil, false, err
	}
	if order.CollectionOwner != string(billing.OrderCollectionEngine) {
		return nil, false, orders.ErrNotPayable
	}
	if order.Origin == string(billing.OrderOriginUsage) {
		payment, created, err = s.moneyService().RecordUsageRemittance(ctx, money.UsageRemittance{OrderID: order.ID, Amount: params.Amount, TransactionID: params.TransactionID, PaidAt: params.PaidAt})
		if errors.Is(err, money.ErrUsageActionNotAllowed) {
			err = errUsageActionRefused
		}
		return payment, created, err
	}
	return rt.Orders.RecordRemittance(ctx, orders.Remittance{OrderID: params.OrderID.UUID(), Amount: params.Amount, TransactionID: params.TransactionID, PaidAt: params.PaidAt})
}

// Merchant orders: the merchant's backend prices lines for a customer and
// creates an unpaid order they pay on the merchant's own site. Merchants
// never pay: no merchant route takes a payment.

var errMerchantOrderExpiry = apperr.Invalidf("expires_at is between an hour and 120 days away").WithParam("expires_at")

// PreviewMerchantOrder prices lines for one customer without writing.
func (s *Service) PreviewMerchantOrder(ctx context.Context, params billing.PreviewOrderParams) (*billing.OrderPreview, error) {
	if params.CustomerID.IsZero() {
		return nil, apperr.Invalidf("customer_id required").WithParam("customer_id")
	}
	return s.PreviewOrder(ctx, OrderActor{Customer: params.CustomerID}, billing.PreviewMyOrderParams{Lines: params.Lines})
}

// CreateMerchantOrder creates an unpaid order for a customer. key is the
// request's Idempotency-Key, kept on the order: the same key answers the same
// order (replayed), another body with it is idempotency_key_reused.
func (s *Service) CreateMerchantOrder(ctx context.Context, params billing.CreateOrderParams, key string) (*billing.Order, bool, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return nil, false, err
	}
	if params.CustomerID.IsZero() {
		return nil, false, apperr.Invalidf("customer_id required").WithParam("customer_id")
	}
	if err := orders.CheckLines(params.Lines); err != nil {
		return nil, false, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, false, apperr.New(http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required")
	}
	if err := s.orderCustomerAllowed(ctx, params.CustomerID); err != nil {
		return nil, false, err
	}
	digest, err := s.orderCreationDigest(ctx, params, params.Lines)
	if err != nil {
		return nil, false, err
	}
	scoped := "app:" + key
	existing, err := rt.Orders.ByIdempotencyKey(ctx, params.CustomerID.UUID(), scoped)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if string(existing.RequestDigest) != string(digest) {
			return nil, false, errOrderKeyReused
		}
		existing, err = rt.Orders.ExpireIfDue(ctx, existing)
		if err != nil {
			return nil, true, err
		}
		view, err := s.merchantOrderView(ctx, existing)
		if err != nil {
			return nil, true, err
		}
		if view.PayURLExpiresAt != nil {
			recovered, err := paymentpage.ReplayURL(ctx, rt.DB.Gen(ctx), s.rt.PaymentOrigin(), existing.BillingOrder, s.now())
			if err != nil {
				return nil, true, err
			}
			if recovered != "" {
				view.PayURL = &recovered
			}
		}
		return view, true, nil
	}
	now := s.now()
	ttl := orders.MerchantTTL
	if params.ExpiresAt != nil {
		ttl = params.ExpiresAt.Sub(now)
		if ttl < time.Hour || ttl > orders.MaxMerchantTTL {
			return nil, false, errMerchantOrderExpiry
		}
	}
	// A pay URL is checked before the order exists; without an expiry of
	// its own, the order expires with it (Stripe).
	var attach func(context.Context, *gen.Queries, uuid.UUID) error
	var payURL string
	if params.PayURL != nil {
		origin := s.rt.PaymentOrigin()
		if origin == "" {
			return nil, false, apperr.New(http.StatusBadRequest, "pay_url_unavailable", "this deployment serves no payment origin").WithParam("pay_url")
		}
		plan, err := paymentpage.Plan(*params.PayURL, billing.OrderID{}, now, params.ExpiresAt)
		if err != nil {
			return nil, false, err
		}
		if plan.Embedded {
			if _, err := s.embedOrigins(ctx); err != nil {
				if errors.Is(err, errEmbedOriginsRequired) {
					err = errEmbedOriginsRequired.WithParam("pay_url.embedded")
				}
				return nil, false, err
			}
		}
		if params.ExpiresAt == nil {
			ttl = plan.ExpiresAt.Sub(now)
		}
		attach = func(ctx context.Context, q *gen.Queries, id uuid.UUID) error {
			mid, err := merchant.Require(ctx)
			if err != nil {
				return err
			}
			c, err := paymentpage.Plan(*params.PayURL, billing.OrderID(id), now, params.ExpiresAt)
			if err != nil {
				return err
			}
			payURL, err = paymentpage.Attach(ctx, q, origin, mid, c, now)
			return err
		}
	}
	order, err := rt.Orders.Create(ctx, orders.CreateInput{CustomerID: params.CustomerID.UUID(), Origin: billing.OrderOriginMerchant, Lines: params.Lines,
		IdempotencyKey: scoped, RequestDigest: digest, TTL: ttl, Attach: attach})
	if err != nil {
		if db.IsUniqueViolation(err) {
			return nil, false, ErrIdempotencyKeyInUse
		}
		return nil, false, err
	}
	if params.PayURL != nil {
		// A Stripe PSP declared before the payment origin was served is
		// registered as its payment-method domain now.
		if mid, err := merchant.Require(ctx); err == nil {
			s.rt.Go("payment domain registration", func(ctx context.Context) {
				if err := s.rt.RegisterPaymentDomain(ctx, mid); err != nil && ctx.Err() == nil {
					log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).Warn("payment domain: cannot queue its registration")
				}
			})
		}
	}
	view, err := s.merchantOrderView(ctx, order)
	if err == nil && payURL != "" && view.PayURLExpiresAt != nil {
		// Only creation and its authenticated retry return the capability.
		view.PayURL = &payURL
	}
	return view, false, err
}

// embedOrigins are the origins the merchant registered to frame its
// embedded pay URLs: embed_origins_required without one.
func (s *Service) embedOrigins(ctx context.Context) ([]string, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if s.rt.Merchants == nil {
		return nil, errEmbedOriginsRequired
	}
	_, settings, err := s.rt.Merchants.MerchantSettings(ctx, mid)
	if err != nil {
		return nil, err
	}
	origins := merchantconfig.EmbedOrigins(settings)
	if len(origins) == 0 {
		return nil, errEmbedOriginsRequired
	}
	return origins, nil
}

var errEmbedOriginsRequired = apperr.New(http.StatusBadRequest, "embed_origins_required",
	"an embedded pay URL needs the origins that may frame it: the merchant's settings payment_page.embed_origins")

// payURLOrigin is the payment origin a customer's order's pay URL is on,
// checked before the order exists: the merchant registered the origins that
// frame it. The request names no URL.
func (s *Service) payURLOrigin(ctx context.Context, params billing.CreateMyOrderParams) (string, error) {
	switch {
	case params.Payment != nil:
		return "", apperr.Invalidf("pay_url and payment are exclusive: the pay URL takes the payment").WithParam("pay_url")
	case !params.PayURL.Embedded:
		return "", apperr.Invalidf("pay_url.embedded is true: a customer's pay URL is framed by the merchant's registered origins").WithParam("pay_url.embedded")
	}
	origin := s.rt.PaymentOrigin()
	if origin == "" {
		return "", apperr.New(http.StatusBadRequest, "pay_url_unavailable", "this deployment serves no payment origin").WithParam("pay_url")
	}
	if _, err := s.embedOrigins(ctx); err != nil {
		return "", err
	}
	return origin, nil
}

// attachPayURL gives the answer's order, read as it is now (a retry's
// answer is the first's), its embedded pay URL in place of the one it had:
// a fresh URL on every answer. A paid or closed order takes none.
func (s *Service) attachPayURL(ctx context.Context, rt *orderRuntime, actor OrderActor, answer OrderAnswer, origin string) (OrderAnswer, error) {
	live, err := s.orderView(ctx, rt, actor, answer.Order.ID.UUID())
	if err != nil {
		return OrderAnswer{}, err
	}
	order := *live
	answer.Order = &order
	if order.Status != billing.OrderOpen || order.CollectionOwner != billing.OrderCollectionEngine {
		return answer, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return OrderAnswer{}, err
	}
	c, url, ok, err := paymentpage.AttachMine(ctx, rt.DB.Gen(ctx), origin, mid, actor.Customer, order.ID, order.ExpiresAt, s.now())
	switch {
	case err != nil:
		return OrderAnswer{}, err
	case !ok:
		return answer, nil
	}
	s.rt.Go("payment domain registration", func(ctx context.Context) {
		if err := s.rt.RegisterPaymentDomain(ctx, mid); err != nil && ctx.Err() == nil {
			log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).Warn("payment domain: cannot queue its registration")
		}
	})
	c.Show(&order, &url)
	return answer, nil
}

// CancelMerchantOrder withdraws an open order the merchant created.
func (s *Service) CancelMerchantOrder(ctx context.Context, id billing.OrderID) (*billing.Order, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return nil, err
	}
	order, err := rt.Orders.CancelMerchantOrder(ctx, id.UUID())
	if err != nil {
		return nil, err
	}
	return s.merchantOrderView(ctx, order)
}

// merchantOrderView is an order as the merchant reads it: what can pay it,
// never a next action (only its customer pays).
func (s *Service) merchantOrderView(ctx context.Context, order *orders.Order) (*billing.Order, error) {
	options, err := s.openOrderOptions(ctx, order)
	if err != nil {
		return nil, err
	}
	view := order.View(nil, options)
	return &view, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
