package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// An order paid on Stripe's hosted page (Stripe Checkout): paying opens an
// attempt and a payment-mode Checkout Session for the order's total, and the
// order awaits the customer with a redirect_to_url next action. Stripe sends
// them back to return_url. The session's completion (its webhook, or confirm
// reading it) completes the order; its expiry or failed payment returns the
// order to requires_payment_method. Closing the order expires the session.
// One-time lines only: a recurring line needs a saved card.

// OrderHosted pays on the hosted page of the Stripe PSP named (zero: the only
// one that takes the order), returning to ReturnURL.
type OrderHosted struct {
	PSPID     uuid.UUID
	ReturnURL string
}

// hostedFlow marks an order attempt paid on a provider's hosted page.
const hostedFlow = "hosted"

// payOrderHosted opens the key's hosted attempt on the order; a key already
// used answers where its attempt stands.
func (s *OrderAttemptService) payOrderHosted(ctx context.Context, in OrderPayInput) (OrderCharge, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return OrderCharge{}, err
	}
	order := in.Order
	attemptID := OrderAttemptID(mid.UUID(), order.ID, in.Key)
	prior, err := s.db.Gen(ctx).GetOrderAttempt(ctx, gen.GetOrderAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
	switch {
	case err == nil:
		if railStateText(prior.RailState, "flow") != hostedFlow || prior.OrderID == nil || *prior.OrderID != order.ID {
			return OrderCharge{}, fmt.Errorf("%w: %w", ErrOrderAttemptConflict, billing.ErrIdempotencyKeyReused)
		}
		return OrderCharge{}, nil
	case !db.IsNotFound(err):
		return OrderCharge{}, err
	}
	if err := s.requireProviderWrites(ctx); err != nil {
		return OrderCharge{}, err
	}
	returnURL := strings.TrimSpace(in.Hosted.ReturnURL)
	if returnURL == "" {
		return OrderCharge{}, fmt.Errorf("%w: return_url is required", ErrOrderAttemptValidation)
	}
	if err := s.validateReturnURLs(returnURL); err != nil {
		return OrderCharge{}, err
	}
	if err := moneyutil.RequireFiatCurrency(order.Currency); err != nil {
		return OrderCharge{}, fmt.Errorf("%w: %v", ErrOrderAttemptValidation, err)
	}
	purchase, ok := s.purchaseService.(*PurchaseService)
	if !ok {
		return OrderCharge{}, errors.New("hosted order payment is unavailable")
	}
	options, err := s.OrderOptions(ctx, in.Prices, in.Products)
	if err != nil {
		return OrderCharge{}, err
	}
	option, err := HostedOption(options, in.Hosted.PSPID)
	if err != nil {
		return OrderCharge{}, err
	}
	minor, err := moneyutil.NativeToRailMinorExact(order.Currency, order.Total)
	if err != nil {
		return OrderCharge{}, err
	}
	raw, err := json.Marshal(map[string]any{"flow": hostedFlow, "return_url": returnURL})
	if err != nil {
		return OrderCharge{}, err
	}
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := s.db.NewWithPgxTx(tx).Gen(ctx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: order.CustomerID}); err != nil {
			return err
		}
		current, err := q.LockOrder(ctx, gen.LockOrderParams{MerchantID: mid.UUID(), ID: order.ID})
		if err != nil {
			return err
		}
		now := s.now().UTC().Truncate(time.Microsecond)
		switch {
		case current.Status == string(billing.OrderProcessing) || current.PaymentStatus == string(billing.OrderPaymentRequiresAction):
			return orders.ErrInProgress
		case current.CollectionOwner != string(billing.OrderCollectionEngine):
			return orders.ErrNotPayable
		case !orders.Payable(current, now):
			return orders.ErrNotPayable
		}
		if err := s.orders.StillBuyable(ctx, q, order, now); err != nil {
			return err
		}
		expires := now.Add(orderAttemptTTL)
		if current.ExpiresAt != nil && current.ExpiresAt.Before(expires) {
			expires = *current.ExpiresAt
		}
		if err := q.CreateOrderAttempt(ctx, gen.CreateOrderAttemptParams{ID: attemptID, MerchantID: mid.UUID(), CustomerID: order.CustomerID, OrderID: order.ID,
			Rail: option.Rail, Status: string(models.OrderAttemptStatusRequiresAction), Amount: current.Total, Currency: current.Currency,
			ExpiresAt: expires, RailState: raw, PspID: option.PSPID, Now: now}); err != nil {
			return err
		}
		if n, err := q.StartOrderAttempt(ctx, gen.StartOrderAttemptParams{MerchantID: mid.UUID(), ID: order.ID, AttemptID: attemptID, PspID: option.PSPID, Now: now}); err != nil || n != 1 {
			return errors.Join(err, orders.ErrNotPayable)
		}
		n, err := q.SetOrderPending(ctx, gen.SetOrderPendingParams{MerchantID: mid.UUID(), ID: order.ID, AttemptID: attemptID, PaymentStatus: string(billing.OrderPaymentRequiresAction), Now: now})
		if err == nil && n != 1 {
			err = orders.ErrNotPayable
		}
		return err
	})
	if err != nil {
		return OrderCharge{}, err
	}
	session, err := purchase.createStripeCheckoutSession(db.WithPSPID(ctx, option.PSPID), stripeCheckoutParams{
		InlinePrice: stripeCheckoutInlinePrice{Name: order.Items(), Currency: order.Currency, AmountMinor: moneyutil.Cents(minor)},
		SuccessURL:  returnURL, CancelURL: returnURL, UserID: order.CustomerID.String(), CustomerEmail: userEmail(in.User),
		OrderAttemptID: billing.OrderAttemptID(attemptID).String(), OrderID: billing.OrderID(order.ID).String(), IdempotencyKey: "order:" + attemptID.String(),
	})
	if err != nil {
		// No page to send the customer to: the order takes another payment.
		failure := &billing.PaymentFailure{Reason: "processing_error", Message: "The payment page could not be opened. Try again."}
		if derr := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return s.orders.Declined(ctx, s.db.NewWithPgxTx(tx), order.ID, attemptID, failure)
		}); derr != nil {
			return OrderCharge{}, errors.Join(err, derr)
		}
		return OrderCharge{Failed: true}, nil
	}
	state, err := json.Marshal(map[string]any{"checkout_session_id": session.ID, "checkout_url": session.URL})
	if err != nil {
		return OrderCharge{}, err
	}
	_, err = s.db.Gen(ctx).SetOrderAttemptStatus(ctx, gen.SetOrderAttemptStatusParams{MerchantID: mid.UUID(), ID: attemptID,
		Status: string(models.OrderAttemptStatusRequiresAction), RailState: state, Now: s.now().UTC()})
	return OrderCharge{}, err
}

// CheckReturnURL refuses a return URL off the host's allowed origins.
func (s *OrderAttemptService) CheckReturnURL(raw string) error {
	return s.validateReturnURLs(raw)
}

// HostedOption is the hosted-page PSP that takes the order: the one named, or
// the only one.
func HostedOption(options []OrderOption, psp uuid.UUID) (*OrderOption, error) {
	var found []*OrderOption
	for i := range options {
		if options[i].Hosted && (psp == uuid.Nil || options[i].PSPID == psp) {
			found = append(found, &options[i])
		}
	}
	switch len(found) {
	case 0:
		return nil, orders.ErrOptionMissing
	case 1:
		return found[0], nil
	}
	return nil, ErrOrderPSPRequired
}

// hostedAttempt reports an order attempt paid on a hosted page.
func hostedAttempt(attempt gen.BillingOrderAttempt) bool {
	return railStateText(attempt.RailState, "flow") == hostedFlow
}

// orderHostedAction sends the customer to the attempt's hosted page.
func orderHostedAction(attempt gen.BillingOrderAttempt) *billing.NextAction {
	link := railStateText(attempt.RailState, "checkout_url")
	if link == "" {
		return nil
	}
	psp := billing.PSPID(attempt.PspID)
	return &billing.NextAction{Type: "redirect_to_url", URL: &link, PSPID: &psp, Transactions: []string{}, Payload: map[string]string{}}
}

// confirmOrderHosted reads the attempt's Checkout Session: a paid one
// completes the order, a closed one returns it to requires_payment_method.
func (s *OrderAttemptService) confirmOrderHosted(ctx context.Context, attempt gen.BillingOrderAttempt) (OrderCharge, error) {
	id := railStateText(attempt.RailState, "checkout_session_id")
	if id == "" {
		return OrderCharge{}, nil
	}
	session, err := s.stripeCheckoutSession(db.WithPSPID(ctx, attempt.PspID), id, false)
	if err != nil {
		return OrderCharge{}, err
	}
	switch {
	case session.Status == "complete" && (session.PaymentStatus == "paid" || session.PaymentStatus == "no_payment_required"):
		return OrderCharge{}, s.SettleOrderHosted(ctx, attempt.ID, session.ID, session.PaymentIntent, session.Currency, session.AmountTotal)
	case session.Status == "expired":
		return OrderCharge{Failed: true}, s.CloseOrderHosted(ctx, attempt.ID, false)
	}
	return OrderCharge{}, nil
}

// abandonOrderHosted expires the Checkout Session of a closed order's
// attempt. A session paid first completes the order late instead.
func (s *OrderAttemptService) abandonOrderHosted(ctx context.Context, attempt gen.BillingOrderAttempt) error {
	id := railStateText(attempt.RailState, "checkout_session_id")
	if id == "" {
		return nil
	}
	session, err := s.stripeCheckoutSession(db.WithPSPID(ctx, attempt.PspID), id, true)
	if err != nil {
		return err
	}
	if session.Status == "complete" && session.PaymentStatus == "paid" {
		return s.SettleOrderHosted(ctx, attempt.ID, session.ID, session.PaymentIntent, session.Currency, session.AmountTotal)
	}
	return nil
}

// SettleOrderHosted completes the order a paid Checkout Session pays, in
// one transaction: the payment, completion and fulfilment. A replay of the
// same session answers the completed order.
func (s *OrderAttemptService) SettleOrderHosted(ctx context.Context, attemptID uuid.UUID, sessionID, paymentIntent, currency string, amountMinor int64) error {
	if s.orders == nil {
		return errors.New("order settlement is unavailable")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	attempt, err := s.db.Gen(ctx).GetOrderAttempt(ctx, gen.GetOrderAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
	if err != nil {
		return err
	}
	if !hostedAttempt(attempt) || attempt.OrderID == nil || railStateText(attempt.RailState, "checkout_session_id") != sessionID {
		return fmt.Errorf("checkout session %s does not pay order attempt %s", sessionID, attemptID)
	}
	if attempt.Amount == nil || attempt.Currency == nil || !strings.EqualFold(*attempt.Currency, currency) {
		return fmt.Errorf("checkout session %s currency %s is not the order's", sessionID, currency)
	}
	minor, err := moneyutil.NativeToRailMinorExact(*attempt.Currency, *attempt.Amount)
	if err != nil {
		return err
	}
	if int64(minor) != amountMinor {
		return fmt.Errorf("checkout session %s amount %d is not the order's %d", sessionID, amountMinor, minor)
	}
	transaction := strings.TrimSpace(paymentIntent)
	if transaction == "" {
		transaction = sessionID
	}
	payment := uuid.NewSHA1(uuid.NameSpaceURL, []byte("openrails:order-payment:"+attemptID.String()+":"+transaction))
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.orders.Paid(db.WithPSPID(ctx, attempt.PspID), s.db.NewWithPgxTx(tx), *attempt.OrderID, orders.Charge{PaymentID: payment, AttemptID: attemptID, PSPID: attempt.PspID,
			Rail: string(models.RailStripe), TransactionID: transaction, Amount: *attempt.Amount, Currency: *attempt.Currency, PurchasedAt: s.now().UTC(),
			Metadata: map[string]any{"stripe_checkout_session_id": sessionID, "stripe_payment_intent_id": paymentIntent, "order_attempt_id": attemptID.String()}})
		return err
	})
}

// CloseOrderHosted ends an attempt whose Checkout Session expired (or whose
// delayed payment failed): its order takes another payment.
func (s *OrderAttemptService) CloseOrderHosted(ctx context.Context, attemptID uuid.UUID, failed bool) error {
	if s.orders == nil {
		return errors.New("order settlement is unavailable")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	attempt, err := s.db.Gen(ctx).GetOrderAttempt(ctx, gen.GetOrderAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
	if err != nil || attempt.OrderID == nil || !hostedAttempt(attempt) {
		return err
	}
	failure := &billing.PaymentFailure{Reason: "payment_page_expired", Message: "The payment page expired before you paid."}
	if failed {
		failure = &billing.PaymentFailure{Reason: "generic_decline", Message: "The payment was declined."}
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return s.orders.Declined(ctx, s.db.NewWithPgxTx(tx), *attempt.OrderID, attemptID, failure)
	})
}

// stripeCheckoutSession reads (or, with expire, expires) a Checkout Session
// on the context's Stripe PSP.
func (s *OrderAttemptService) stripeCheckoutSession(ctx context.Context, id string, expire bool) (stripeCheckoutSession, error) {
	purchase, ok := s.purchaseService.(*PurchaseService)
	if !ok || purchase.StripeClients == nil {
		return stripeCheckoutSession{}, errors.New("stripe is unavailable")
	}
	_, secret, err := subscriptions.RequireStripeSecretKey(ctx, purchase.Rails)
	if err != nil {
		return stripeCheckoutSession{}, err
	}
	method, path := http.MethodGet, "https://api.stripe.com/v1/checkout/sessions/"+id
	if expire {
		method, path = http.MethodPost, path+"/expire"
	}
	req, err := http.NewRequestWithContext(ctx, method, path, nil)
	if err != nil {
		return stripeCheckoutSession{}, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	if expire {
		stripeapi.SetIdempotencyKey(req, stripeCheckoutIdempotencyKey("expire:"+id))
	}
	resp, err := purchase.StripeClients.Client(purchase.Config, 0).Do(req)
	if err != nil {
		return stripeCheckoutSession{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		// A session no longer open cannot be expired: read where it stands.
		if expire && resp.StatusCode == http.StatusBadRequest {
			return s.stripeCheckoutSession(ctx, id, false)
		}
		return stripeCheckoutSession{}, fmt.Errorf("stripe checkout session %s: %s", id, parseStripeError(body))
	}
	var out stripeCheckoutSession
	return out, json.Unmarshal(body, &out)
}
