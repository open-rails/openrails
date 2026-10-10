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
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Orders (#1168): a customer previews, buys (one call, or create then pay),
// confirms after acting, cancels and reads their own orders. The order id is
// not a credential: every action is the customer's own, and another
// customer's order does not exist.

var (
	// ErrIdempotencyKeyInUse: the request first sent with the key is still
	// running.
	ErrIdempotencyKeyInUse = apperr.New(http.StatusConflict, billing.CodeIdempotencyKeyInUse, "The request first sent with this Idempotency-Key is still running.")
	errOrderKeyReused      = apperr.New(http.StatusUnprocessableEntity, billing.CodeIdempotencyKeyReused, "The Idempotency-Key was first sent with a different request.")
	errOrderCustomerBlock  = apperr.New(http.StatusForbidden, "customer_blocked", "This customer may not buy.")
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

type orderKeyResult struct {
	OrderID uuid.UUID `json:"order_id"`
	Digest  []byte    `json:"digest"`
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

// CreateOrder freezes lines into an order and, with a payment, pays it in the
// same call. key is the request's Idempotency-Key; replayed reports an answer
// to an earlier request with the key, which reads the order's current state.
func (s *Service) CreateOrder(ctx context.Context, actor OrderActor, params billing.CreateOrderParams, key string) (out *billing.Order, replayed bool, err error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return nil, false, err
	}
	if err := orders.CheckLines(params.Lines); err != nil {
		return nil, false, err
	}
	if params.Payment != nil && params.ExpectedTotal == nil {
		return nil, false, apperr.Invalidf("expected_total is required with a payment").WithParam("expected_total")
	}
	if params.Payment != nil && params.Payment.PaymentMethodID.IsZero() {
		return nil, false, apperr.Invalidf("payment_method_id required").WithParam("payment.payment_method_id")
	}
	if err := s.orderCustomerAllowed(ctx, actor.Customer); err != nil {
		return nil, false, err
	}
	digest := orderDigest(params)
	customer := actor.Customer.UUID()
	scoped := customer.String() + ":" + key
	claim, rec, err := rt.Idempotency.Begin(ctx, orderCreateOperation, scoped)
	if err != nil {
		return nil, false, err
	}
	if claim == nil {
		if rec.Status != idempotency.StatusSucceeded {
			return nil, false, ErrIdempotencyKeyInUse
		}
		var result orderKeyResult
		if err := json.Unmarshal(rec.Result, &result); err != nil {
			return nil, false, err
		}
		if string(result.Digest) != string(digest) {
			return nil, false, errOrderKeyReused
		}
		order, err := s.resumeCreate(ctx, rt, actor, result.OrderID, params, key)
		return order, true, err
	}
	work, stop := claim.Hold(ctx)
	work = db.WithCommitGuard(work, claim.InTx)
	id, replayed, err := s.createClaimed(work, rt, actor, params, key, digest)
	stop() // before settling: a renewal never races the final state
	if errors.Is(context.Cause(work), idempotency.ErrClaimLost) || errors.Is(err, idempotency.ErrClaimLost) {
		return nil, false, ErrIdempotencyKeyInUse
	}
	s.settleOrderClaim(ctx, claim, id, digest, err)
	if err != nil {
		return nil, false, err
	}
	view, err := s.orderView(ctx, rt, actor, id)
	return view, replayed, err
}

// createClaimed runs a create under its key's claim: a key the order table
// already maps (its claim long gone) answers that order.
func (s *Service) createClaimed(ctx context.Context, rt *orderRuntime, actor OrderActor, params billing.CreateOrderParams, key string, digest []byte) (uuid.UUID, bool, error) {
	customer := actor.Customer.UUID()
	existing, err := rt.Orders.ByIdempotencyKey(ctx, customer, key)
	if err != nil {
		return uuid.Nil, false, err
	}
	if existing != nil {
		if string(existing.RequestDigest) != string(digest) {
			return uuid.Nil, false, errOrderKeyReused
		}
		return existing.ID, true, s.resumePayment(ctx, rt, actor, existing.ID, params, key)
	}
	if params.Payment != nil {
		// A payment that cannot be made refuses the order before it exists.
		quote, err := rt.Orders.Preview(ctx, customer, params.Lines)
		if err == nil {
			err = s.orderPaymentUsable(ctx, quote, params.Payment.PaymentMethodID, actor.Customer)
		}
		if err != nil {
			return uuid.Nil, false, err
		}
	}
	order, err := rt.Orders.Create(ctx, orders.CreateInput{CustomerID: customer, Origin: billing.OrderOriginCustomer, Lines: params.Lines,
		ExpectedTotal: params.ExpectedTotal, IdempotencyKey: key, RequestDigest: digest, TTL: orders.CustomerTTL})
	if err != nil {
		return uuid.Nil, false, err
	}
	if params.Payment != nil && order.Status == string(billing.OrderOpen) {
		err = s.payOrder(ctx, rt, actor, order, params.Payment.PaymentMethodID.UUID(), "create:"+key)
	}
	return order.ID, false, err
}

// resumeCreate answers a replayed create: the order now, its payment
// resumed under the same key while the order is open.
func (s *Service) resumeCreate(ctx context.Context, rt *orderRuntime, actor OrderActor, id uuid.UUID, params billing.CreateOrderParams, key string) (*billing.Order, error) {
	if err := s.resumePayment(ctx, rt, actor, id, params, key); err != nil {
		return nil, err
	}
	return s.orderView(ctx, rt, actor, id)
}

// resumePayment runs a one-call buy's payment again under its key: the same
// attempt, which never charges twice.
func (s *Service) resumePayment(ctx context.Context, rt *orderRuntime, actor OrderActor, id uuid.UUID, params billing.CreateOrderParams, key string) error {
	order, err := rt.Orders.GetForCustomer(ctx, actor.Customer.UUID(), id)
	if err != nil || params.Payment == nil || order.Status != string(billing.OrderOpen) {
		return err
	}
	return s.payOrder(ctx, rt, actor, order, params.Payment.PaymentMethodID.UUID(), "create:"+key)
}

func (s *Service) settleOrderClaim(ctx context.Context, claim *idempotency.Claim, order uuid.UUID, digest []byte, err error) {
	if err != nil {
		_ = claim.Fail(ctx, err)
		return
	}
	raw, _ := json.Marshal(orderKeyResult{OrderID: order, Digest: digest})
	_ = claim.Complete(ctx, raw)
}

// PayOrder pays the customer's open order. One key is one attempt.
func (s *Service) PayOrder(ctx context.Context, actor OrderActor, id billing.OrderID, params billing.PayOrderParams, key string) (out *billing.Order, replayed bool, err error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return nil, false, err
	}
	if params.Payment.PaymentMethodID.IsZero() {
		return nil, false, apperr.Invalidf("payment_method_id required").WithParam("payment.payment_method_id")
	}
	if err := s.orderCustomerAllowed(ctx, actor.Customer); err != nil {
		return nil, false, err
	}
	order, err := rt.Orders.GetForCustomer(ctx, actor.Customer.UUID(), id.UUID())
	if err != nil {
		return nil, false, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, false, err
	}
	digest := orderDigest(params)
	scoped := order.ID.String() + ":" + key
	claim, rec, err := rt.Idempotency.Begin(ctx, orderPayOperation, scoped)
	if err != nil {
		return nil, false, err
	}
	if claim == nil {
		if rec.Status != idempotency.StatusSucceeded {
			return nil, false, ErrIdempotencyKeyInUse
		}
		var result orderKeyResult
		if err := json.Unmarshal(rec.Result, &result); err != nil {
			return nil, false, err
		}
		if string(result.Digest) != string(digest) {
			return nil, false, errOrderKeyReused
		}
		view, err := s.orderView(ctx, rt, actor, order.ID)
		return view, true, err
	}
	work, stop := claim.Hold(ctx)
	work = db.WithCommitGuard(work, claim.InTx)
	_, attemptErr := rt.DB.Gen(work).GetOrderAttempt(work, gen.GetOrderAttemptParams{MerchantID: mid.UUID(), ID: checkout.OrderAttemptID(mid.UUID(), order.ID, "pay:"+key)})
	replayed = attemptErr == nil
	if !replayed && params.ExpectedTotal != order.Total {
		err = orders.ErrTotalChanged
	} else {
		err = s.payOrder(work, rt, actor, order, params.Payment.PaymentMethodID.UUID(), "pay:"+key)
	}
	stop() // before settling: a renewal never races the final state
	if errors.Is(context.Cause(work), idempotency.ErrClaimLost) || errors.Is(err, idempotency.ErrClaimLost) {
		return nil, false, ErrIdempotencyKeyInUse
	}
	s.settleOrderClaim(ctx, claim, order.ID, digest, err)
	if err != nil {
		return nil, false, err
	}
	view, err := s.orderView(ctx, rt, actor, order.ID)
	return view, replayed, err
}

func (s *Service) payOrder(ctx context.Context, rt *orderRuntime, actor OrderActor, order *orders.Order, method uuid.UUID, key string) error {
	quote, err := s.frozenQuote(ctx, order)
	if err != nil {
		return err
	}
	user := &checkout.UserIdentity{ID: actor.Customer.String(), ClientIP: actor.ClientIP}
	return rt.Checkout.PayOrder(ctx, checkout.OrderPayInput{Order: order, PaymentMethodID: method, Key: key, User: user, Prices: quote.prices, Products: quote.products})
}

// ConfirmOrder reads the provider after the customer completed next_action.
func (s *Service) ConfirmOrder(ctx context.Context, actor OrderActor, id billing.OrderID) (*billing.Order, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rt, err := s.orderRuntime()
	if err != nil {
		return nil, err
	}
	order, err := rt.Orders.GetForCustomer(ctx, actor.Customer.UUID(), id.UUID())
	if err != nil {
		return nil, err
	}
	if err := rt.Checkout.ConfirmOrder(ctx, order, actor.Principal); err != nil {
		return nil, err
	}
	return s.orderView(ctx, rt, actor, order.ID)
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
	if order.Status == string(billing.OrderRequiresAction) {
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
// that sells its lines can charge.
func (s *Service) orderPaymentUsable(ctx context.Context, quote *orders.Quote, method billing.PaymentMethodID, customer billing.CustomerID) error {
	if i := quote.Refused(); i >= 0 {
		return &orders.LineError{Index: i, Refusal: *quote.Lines[i].Refusal}
	}
	pm, err := s.rt.PaymentMethodService.ValidatePaymentMethodOperation(ctx, method.UUID(), customer.String())
	if err != nil {
		return fmt.Errorf("%w: %w", checkout.ErrPaymentMethodStale, err)
	}
	options, err := s.rt.CheckoutAttemptService.OrderOptions(ctx, quote.Prices(), quote.Products())
	if err != nil {
		return err
	}
	for _, o := range options {
		if strings.EqualFold(string(pm.Rail), o.Rail) && pm.ChargeableOn(o.PSPID) {
			return nil
		}
	}
	return orders.ErrOptionMissing
}

// orderCustomerAllowed refuses a customer the merchant blocked, and every
// customer of a merchant whose provider writes are fenced.
func (s *Service) orderCustomerAllowed(ctx context.Context, customer billing.CustomerID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	if err := s.rt.CheckoutAttemptService.TakesOrders(ctx); err != nil {
		return errOrdersFenced
	}
	row, err := s.rt.DB.Gen(ctx).GetCustomer(ctx, gen.GetCustomerParams{MerchantID: mid.UUID(), ID: customer.UUID()})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return err
	case row.Blocked:
		return errOrderCustomerBlock
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
