package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
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
func (s *Service) PreviewOrder(ctx context.Context, actor OrderActor, params billing.PreviewOrderParams) (*billing.OrderPreview, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return nil, err
	}
	quote, err := rt.Orders.Preview(ctx, actor.Customer.UUID(), params.Lines)
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

// orderPayment is a request's payment: a saved card or a card just entered.
type orderPayment struct {
	method uuid.UUID
	card   *checkout.OrderCard
}

func readOrderPayment(p *billing.OrderPaymentParams, reusable *bool) (orderPayment, error) {
	if p == nil {
		if reusable != nil {
			return orderPayment{}, apperr.Invalidf("reusable applies to a new card").WithParam("reusable")
		}
		return orderPayment{}, nil
	}
	saved, token := p.PaymentMethodID != nil && !p.PaymentMethodID.IsZero(), strings.TrimSpace(p.Token) != ""
	switch {
	case saved == token:
		return orderPayment{}, apperr.Invalidf("payment names exactly one of payment_method_id or token").WithParam("payment")
	case saved && (reusable != nil || p.PSPID != nil || p.BillingDetails != nil):
		return orderPayment{}, apperr.Invalidf("psp_id, billing_details and reusable describe a new card").WithParam("payment")
	case saved:
		return orderPayment{method: p.PaymentMethodID.UUID()}, nil
	}
	card := &checkout.OrderCard{Token: p.Token, Billing: p.BillingDetails, Reusable: reusable}
	if p.PSPID != nil {
		card.PSPID = p.PSPID.UUID()
	}
	return orderPayment{card: card}, nil
}

// CreateOrder freezes lines into an order and, with a payment, pays it in the
// same call. key is the request's Idempotency-Key: a later request with it
// answers what the first did.
func (s *Service) CreateOrder(ctx context.Context, actor OrderActor, params billing.CreateOrderParams, key string) (OrderAnswer, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return OrderAnswer{}, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return OrderAnswer{}, err
	}
	if err := orders.CheckLines(params.Lines); err != nil {
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
	digest := orderDigest(params)
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
	id, replayed, charged, err := s.createClaimed(work, rt, actor, params, pay, key, digest)
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

// createClaimed runs a create under its key's claim: a key the order table
// already maps (its claim long gone) answers that order.
func (s *Service) createClaimed(ctx context.Context, rt *orderRuntime, actor OrderActor, params billing.CreateOrderParams, pay orderPayment, key string, digest []byte) (uuid.UUID, bool, checkout.OrderCharge, error) {
	customer := actor.Customer.UUID()
	existing, err := rt.Orders.ByIdempotencyKey(ctx, customer, key)
	if err != nil {
		return uuid.Nil, false, checkout.OrderCharge{}, err
	}
	if existing != nil {
		if string(existing.RequestDigest) != string(digest) {
			return uuid.Nil, false, checkout.OrderCharge{}, errOrderKeyReused
		}
		charged, err := s.resumePayment(ctx, rt, actor, existing.ID, pay, "create:"+key)
		return existing.ID, true, charged, err
	}
	if params.Payment != nil {
		// A payment that cannot be made refuses the order before it exists.
		quote, err := rt.Orders.Preview(ctx, customer, params.Lines)
		if err == nil {
			err = s.orderPaymentUsable(ctx, quote, pay, actor.Customer)
		}
		if err != nil {
			return uuid.Nil, false, checkout.OrderCharge{}, err
		}
	}
	order, err := rt.Orders.Create(ctx, orders.CreateInput{CustomerID: customer, Origin: billing.OrderOriginCustomer, Lines: params.Lines,
		ExpectedTotal: params.ExpectedTotal, IdempotencyKey: key, RequestDigest: digest, TTL: orders.CustomerTTL})
	if err != nil {
		return uuid.Nil, false, checkout.OrderCharge{}, err
	}
	var charged checkout.OrderCharge
	if params.Payment != nil && order.Status == string(billing.OrderOpen) {
		charged, err = s.payOrder(ctx, rt, actor, order, pay, "create:"+key)
	}
	return order.ID, false, charged, err
}

// resumePayment runs a one-call buy's payment again under its key: the same
// attempt, which never charges twice.
func (s *Service) resumePayment(ctx context.Context, rt *orderRuntime, actor OrderActor, id uuid.UUID, pay orderPayment, key string) (checkout.OrderCharge, error) {
	order, err := rt.Orders.GetForCustomer(ctx, actor.Customer.UUID(), id)
	if err != nil || (pay.method == uuid.Nil && pay.card == nil) || order.Status != string(billing.OrderOpen) {
		return checkout.OrderCharge{}, err
	}
	return s.payOrder(ctx, rt, actor, order, pay, key)
}

// orderAnswer reads the order a request acted on into its answer: 402 when
// its payment failed. err passes through.
func (s *Service) orderAnswer(ctx context.Context, rt *orderRuntime, actor OrderActor, id uuid.UUID, status int, charged checkout.OrderCharge, replayed bool, err error) (OrderAnswer, error) {
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
		if result.Order.Payment.NextAction, err = rt.Checkout.OrderNextAction(ctx, order, actor.Principal, resolver); err != nil {
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
	if err := s.orderCustomerAllowed(ctx, actor.Customer); err != nil {
		return OrderAnswer{}, err
	}
	order, err := rt.Orders.GetForCustomer(ctx, actor.Customer.UUID(), id.UUID())
	if err != nil {
		return OrderAnswer{}, err
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
			return s.orderAnswer(ctx, rt, actor, id, http.StatusOK, checkout.OrderCharge{}, true, nil)
		})
	}
	work, stop := claim.Hold(ctx)
	work = db.WithCommitGuard(work, claim.InTx)
	_, attemptErr := rt.DB.Gen(work).GetOrderAttempt(work, gen.GetOrderAttemptParams{MerchantID: mid.UUID(), ID: checkout.OrderAttemptID(mid.UUID(), order.ID, "pay:"+key)})
	replayed := attemptErr == nil
	var charged checkout.OrderCharge
	if !replayed && params.ExpectedTotal != order.Total {
		err = orders.ErrTotalChanged
	} else {
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

func (s *Service) payOrder(ctx context.Context, rt *orderRuntime, actor OrderActor, order *orders.Order, pay orderPayment, key string) (checkout.OrderCharge, error) {
	quote, err := s.frozenQuote(ctx, order)
	if err != nil {
		return checkout.OrderCharge{}, err
	}
	user := &checkout.UserIdentity{ID: actor.Customer.String(), ClientIP: actor.ClientIP}
	return rt.Checkout.PayOrder(ctx, checkout.OrderPayInput{Order: order, PaymentMethodID: pay.method, Card: pay.card, Key: key, User: user, Prices: quote.prices, Products: quote.products})
}

// ConfirmOrder reads the provider after the customer completed next_action;
// an attempt that failed answers 402.
func (s *Service) ConfirmOrder(ctx context.Context, actor OrderActor, id billing.OrderID) (OrderAnswer, error) {
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
	charged, err := rt.Checkout.ConfirmOrder(ctx, order, actor.Principal)
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
	return &view, nil
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
		return out, nil
	}
	var filter orders.ListFilter
	if !params.CustomerID.IsZero() {
		filter.CustomerID = new(params.CustomerID.UUID())
	}
	if !params.PriceID.IsZero() {
		filter.PriceID = new(params.PriceID.UUID())
	}
	if params.Status != "" {
		filter.Status = new(string(params.Status))
	}
	page, err := rt.Orders.List(ctx, filter, params.PageRequest)
	if err != nil {
		return billing.ListPage[billing.Order]{}, err
	}
	out := billing.ListPage[billing.Order]{Items: make([]billing.Order, len(page.Items)), Next: page.Next}
	for i, o := range page.Items {
		out.Items[i] = o.View(nil, nil)
	}
	return out, nil
}

// orderView is the order as its customer reads it, with the next action of a
// payment awaiting them and the options of an open order.
func (s *Service) orderView(ctx context.Context, rt *orderRuntime, actor OrderActor, id uuid.UUID) (*billing.Order, error) {
	order, err := rt.Orders.GetForCustomer(ctx, actor.Customer.UUID(), id)
	if err != nil {
		return nil, err
	}
	var next *billing.NextAction
	if order.AwaitsCustomer() {
		resolver, _ := s.rt.CollectionResolver.(intents.StripeEngineServiceResolver)
		if next, err = rt.Checkout.OrderNextAction(ctx, order, actor.Principal, resolver); err != nil {
			return nil, err
		}
	}
	var options []billing.OrderPaymentOption
	if order.Status == string(billing.OrderOpen) {
		quote, err := s.frozenQuote(ctx, order)
		if err != nil {
			return nil, err
		}
		if options, err = s.orderOptions(ctx, quote.prices, quote.products); err != nil {
			return nil, err
		}
	}
	view := order.View(next, options)
	return &view, nil
}

type frozenLines struct {
	prices   []*models.Price
	products []*models.Product
}

// frozenQuote reads the catalog rows an order's lines name, for routing.
func (s *Service) frozenQuote(ctx context.Context, order *orders.Order) (frozenLines, error) {
	var out frozenLines
	for _, l := range order.Lines {
		price, err := s.rt.PriceService.GetByID(ctx, l.PriceID)
		if err != nil {
			return out, err
		}
		product, err := s.rt.ProductService.GetByID(ctx, l.ProductID)
		if err != nil {
			return out, err
		}
		out.prices, out.products = append(out.prices, price), append(out.products, product)
	}
	return out, nil
}

func (s *Service) orderOptions(ctx context.Context, prices []*models.Price, products []*models.Product) ([]billing.OrderPaymentOption, error) {
	options, err := s.rt.CheckoutAttemptService.OrderOptions(ctx, prices, products)
	if err != nil {
		return nil, err
	}
	out := make([]billing.OrderPaymentOption, 0, len(options))
	for _, o := range options {
		out = append(out, billing.OrderPaymentOption{PSPID: billing.PSPID(o.PSPID), Rail: o.Rail, Accepts: []string{"saved_card", "new_card"}})
	}
	return out, nil
}

// orderPaymentUsable refuses, before an order exists, a saved card no PSP
// that sells its lines can charge, or a new card's PSP that takes none.
func (s *Service) orderPaymentUsable(ctx context.Context, quote *orders.Quote, pay orderPayment, customer billing.CustomerID) error {
	if i := quote.Refused(); i >= 0 {
		return &orders.LineError{Index: i, Refusal: *quote.Lines[i].Refusal}
	}
	options, err := s.rt.CheckoutAttemptService.OrderOptions(ctx, quote.Prices(), quote.Products())
	if err != nil {
		return err
	}
	if pay.card != nil {
		_, err := checkout.NewCardOption(options, pay.card.PSPID)
		return err
	}
	pm, err := s.rt.PaymentMethodService.ValidatePaymentMethodOperation(ctx, pay.method, customer.String())
	if err != nil {
		return fmt.Errorf("%w: %w", checkout.ErrPaymentMethodStale, err)
	}
	if pm.Status != paymentmethods.StatusActive {
		return fmt.Errorf("%w: the card is %s", checkout.ErrPaymentMethodStale, pm.Status)
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
	if err := s.rt.CheckoutAttemptService.TakesOrders(ctx); err != nil {
		return errOrdersFenced
	}
	return nil
}

type orderRuntime struct {
	Orders      *orders.Service
	Checkout    *checkout.CheckoutAttemptService
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
	if rt.Orders == nil || rt.CheckoutAttemptService == nil || rt.Idempotency == nil || rt.DB == nil || rt.PriceService == nil || rt.ProductService == nil || rt.PaymentMethodService == nil {
		return nil, errors.New("billing service: orders unavailable")
	}
	return &orderRuntime{Orders: rt.Orders, Checkout: rt.CheckoutAttemptService, Idempotency: rt.Idempotency, DB: rt.DB}, nil
}

func orderDigest(v any) []byte {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return sum[:]
}

// RecordPayment records money the merchant received outside OpenRails for an
// invoice or an order. created is false when the same remittance was already
// recorded.
func (s *Service) RecordPayment(ctx context.Context, params billing.CreatePaymentParams) (payment *models.Payment, created bool, err error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()
	if params.InvoiceID != nil {
		return s.moneyService().RecordInvoiceRemittance(ctx, money.InvoiceRemittance{InvoiceID: params.InvoiceID.UUID(), Amount: params.Amount, TransactionID: params.TransactionID, PaidAt: params.PaidAt})
	}
	rt, err := s.orderRuntime()
	if err != nil {
		return nil, false, err
	}
	return rt.Orders.RecordRemittance(ctx, orders.Remittance{OrderID: params.OrderID.UUID(), Amount: params.Amount, TransactionID: params.TransactionID, PaidAt: params.PaidAt})
}
