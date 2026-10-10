package checkout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/cardholdername"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Orders are paid by checkout attempts (mode order) whose charge is the sale
// intent: the same admission, submission fence, receipt and verification as a
// one-price sale, settling the order's own lines.

// orderAttemptTTL bounds one attempt; the order's own expiry bounds it too.
const orderAttemptTTL = 24 * time.Hour

// SetOrders arms order payment.
func (s *CheckoutAttemptService) SetOrders(o *orders.Service) {
	if s == nil {
		return
	}
	s.orders = o
	if o != nil {
		o.Abandon = s.AbandonOrderAttempt
	}
	if sale := s.saleService(); sale != nil {
		sale.Orders = o
	}
}

func (s *CheckoutAttemptService) saleService() *CheckoutNMISaleService {
	cs, ok := s.checkoutService.(*CheckoutService)
	if !ok || cs == nil {
		return nil
	}
	return cs.NMISaleService
}

// OrderOption is one PSP that can take every line of an order.
type OrderOption struct {
	PSPID uuid.UUID
	Rail  string
}

// OrderOptions are the PSPs that sell every line on rails OpenRails charges
// itself: a saved card on NMI or Stripe, every recurring line an
// engine-owned subscription.
func (s *CheckoutAttemptService) OrderOptions(ctx context.Context, prices []*models.Price, products []*models.Product) ([]OrderOption, error) {
	if len(prices) == 0 || len(prices) != len(products) {
		return []OrderOption{}, nil
	}
	var out []OrderOption
	for i, price := range prices {
		decision, err := s.Route(ctx, RoutingInput{Price: price, Product: products[i], Mode: checkoutModeForRail(price, "")})
		if err != nil && !errors.Is(err, ErrNoRoutableProcessor) {
			return nil, err
		}
		var line []OrderOption
		for _, c := range decision.Candidates {
			rail := models.Rail(c.Rail)
			if c.Skip != "" || c.PSPID == uuid.Nil || (rail != models.RailNMI && rail != models.RailStripe) {
				continue
			}
			if price.IsRecurring() && rails.NewSubscriptionFor(rail) != rails.NewSubscriptionEngine {
				continue
			}
			if i == 0 || containsOption(out, c.PSPID) {
				line = append(line, OrderOption{PSPID: c.PSPID, Rail: c.Rail})
			}
		}
		out = line
	}
	if out == nil {
		out = []OrderOption{}
	}
	return out, nil
}

func containsOption(options []OrderOption, psp uuid.UUID) bool {
	for _, o := range options {
		if o.PSPID == psp {
			return true
		}
	}
	return false
}

// OrderPayInput pays an order with a saved card (PaymentMethodID) or a card
// just entered (Card). Key is the request's scoped Idempotency-Key: one key
// is one attempt, forever.
type OrderPayInput struct {
	Order           *orders.Order
	PaymentMethodID uuid.UUID
	Card            *OrderCard
	Key             string
	User            *UserIdentity
	Prices          []*models.Price
	Products        []*models.Product
}

// OrderCard is a card just entered in a PSP's own fields: its single-use
// token, the PSP whose fields made it (zero: the one option that takes new
// cards), its billing details and the customer's reuse choice.
type OrderCard struct {
	Token    string
	PSPID    uuid.UUID
	Billing  *billing.BillingDetails
	Reusable *bool
}

// OrderCharge is what paying did: Failed when its attempt ended without
// moving money, Declined when the issuer declined it.
type OrderCharge struct{ Failed, Declined bool }

// ErrOrderPSPRequired: more than one PSP takes new cards for the order.
var ErrOrderPSPRequired = apperr.Invalidf("more than one PSP takes new cards for this order: name payment.psp_id").WithParam("payment.psp_id")

// TakesOrders refuses while the merchant's provider writes are fenced or
// held: such a merchant takes no orders.
func (s *CheckoutAttemptService) TakesOrders(ctx context.Context) error {
	return s.requireProviderWrites(ctx)
}

// OrderAttemptID is the attempt a key makes on an order.
func OrderAttemptID(merchantID, orderID uuid.UUID, key string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("openrails:order-attempt:"+merchantID.String()+":"+orderID.String()+":"+key))
}

// PayOrder starts (or, for the same key, resumes) one attempt on the order
// and runs its charge inline. The order reflects the outcome: complete, open
// with last_payment_error, awaiting the customer's action, or processing. A
// new card is charged and saved in one provider request.
func (s *CheckoutAttemptService) PayOrder(ctx context.Context, in OrderPayInput) (OrderCharge, error) {
	if in.Order == nil || in.User == nil || in.Key == "" || (in.PaymentMethodID == uuid.Nil) == (in.Card == nil) {
		return OrderCharge{}, errors.New("order payment needs its order, customer, key and one payment")
	}
	if err := s.guardCardAttempt(ctx, in.User); err != nil {
		return OrderCharge{}, err
	}
	out, err := s.payOrder(ctx, in)
	if out.Declined || err != nil && CardAttemptFailed(nil, err) {
		s.noteCardAttempt(ctx, in.User, &CheckoutAttemptResponse{Status: "failed"}, nil)
	}
	return out, err
}

// newCard is a card just entered, as the order's charge saves it.
type newCard struct {
	instrument  charge.FrozenInstrument
	token       string
	billing     *payments.NewCardBilling
	card        *models.Card
	fingerprint string
	reuse       bool
	// saved is the customer's own method for the card, when they saved it
	// before: the order charges that instead.
	saved uuid.UUID
}

func (s *CheckoutAttemptService) payOrder(ctx context.Context, in OrderPayInput) (OrderCharge, error) {
	sale := s.saleService()
	checkout, ok := s.checkoutService.(*CheckoutService)
	if sale == nil || sale.Intents == nil || s.orders == nil || !ok {
		return OrderCharge{}, errors.New("order payment is unavailable")
	}
	if err := s.requireProviderWrites(ctx); err != nil {
		return OrderCharge{}, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return OrderCharge{}, err
	}
	order := in.Order
	attemptID := OrderAttemptID(mid.UUID(), order.ID, in.Key)
	fingerprint := orderSaleFingerprint(order.ID, in, in.Key)
	operationKey := NMISaleIdempotencyKey("order:" + attemptID.String())
	owns := func(intent gen.BillingProviderIntent) error {
		p, err := payments.DecodeNMISalePayload(intent)
		if err != nil {
			return err
		}
		if p.OrderID != order.ID || p.CheckoutAttemptID != attemptID || p.UserID != order.CustomerID.String() || p.RequestFingerprint != fingerprint {
			return fmt.Errorf("%w: %w", ErrCheckoutAttemptConflict, billing.ErrIdempotencyKeyReused)
		}
		return nil
	}
	prior, err := intents.NewStore(s.db).GetByIdempotencyKey(ctx, operationKey)
	switch {
	case err == nil:
		if err := owns(prior); err != nil {
			return OrderCharge{}, err
		}
		intent, err := sale.Intents.EnqueueOwnedAndExecute(ctx, saleReplayParams(prior), owns)
		if err != nil {
			return OrderCharge{}, err
		}
		return s.afterOrderCharge(ctx, order, attemptID, intent)
	case !db.IsNotFound(err):
		return OrderCharge{}, err
	}

	options, err := s.OrderOptions(ctx, in.Prices, in.Products)
	if err != nil {
		return OrderCharge{}, err
	}
	var chosen *OrderOption
	var card *newCard
	methodID := in.PaymentMethodID
	if in.Card != nil {
		if chosen, err = NewCardOption(options, in.Card.PSPID); err != nil {
			return OrderCharge{}, err
		}
		if card, err = s.prepareNewCard(ctx, checkout, sale, in, *chosen); err != nil {
			return OrderCharge{}, err
		}
		if card.saved != uuid.Nil {
			methodID, card = card.saved, nil
		}
	}
	if card == nil {
		method, err := s.paymentMethodService.ValidatePaymentMethodOperation(ctx, methodID, in.User.ID)
		if err != nil {
			if errors.Is(err, paymentmethods.ErrPaymentMethodNotFound) || errors.Is(err, paymentmethods.ErrPaymentMethodAccessDenied) {
				return OrderCharge{}, fmt.Errorf("%w: %w", ErrPaymentMethodStale, err)
			}
			return OrderCharge{}, err
		}
		if method.Custodian != models.CustodianPSP || method.ParkReason != "" || method.Status != paymentmethods.StatusActive || (method.Rail != models.RailNMI && method.Rail != models.RailStripe) {
			return OrderCharge{}, fmt.Errorf("%w: the payment method cannot pay an order", ErrPaymentMethodStale)
		}
		chosen = nil
		for i, o := range options {
			if rails.SameRail(method.Rail, models.Rail(o.Rail)) && method.ChargeableOn(o.PSPID) {
				chosen = &options[i]
				break
			}
		}
		if chosen == nil {
			return OrderCharge{}, orders.ErrOptionMissing
		}
	}
	target, err := checkout.resolveRailTargetForPSP(ctx, chosen.Rail, chosen.PSPID)
	if err != nil {
		return OrderCharge{}, err
	}
	if err := sale.requireArmed(ctx, target); err != nil {
		return OrderCharge{}, err
	}
	var intent gen.BillingProviderIntent
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		q := d.Gen(ctx)
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
		case current.Status != string(billing.OrderOpen) || !current.ExpiresAt.After(now):
			return orders.ErrNotPayable
		}
		if err := s.orders.StillFree(ctx, q, order, now); err != nil {
			return err
		}
		payload := payments.NMISalePayload{
			CheckoutAttemptID: attemptID, RequestFingerprint: fingerprint, Provider: target.Rail, PSP: target.PSP,
			Amount: current.Total, ListAmount: current.Total, Currency: current.Currency, Description: "Order " + billing.OrderID(order.ID).String(),
			UserID: order.CustomerID.String(), PaymentID: uuidutil.NewV7(), AcceptedAt: now, Eligibility: string(EligibilityAllowed), OrderID: order.ID, Recurring: order.HasRecurring(),
		}
		var charged *uuid.UUID
		if card != nil {
			// The charge saves the card: the method exists once it succeeds.
			payload.PaymentMethodID, payload.Instrument, payload.NewCard = uuidutil.NewV7(), card.instrument, true
			payload.Token, payload.Billing, payload.Card, payload.Fingerprint = card.token, card.billing, card.card, card.fingerprint
			payload.Reuse, payload.PurchaseScoped = card.reuse, !payload.Recurring && !card.reuse
		} else {
			row, err := q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: mid.UUID(), ID: methodID})
			if err != nil {
				return err
			}
			// The customer pays in person: a recurring line opens a recurring
			// agreement, a one-time order reuses the card on file; each cites
			// the card's lineage, or this charge stores it. On Stripe an order
			// that starts a subscription stores its own agreement
			// (setup_future_usage), as an enrollment does, and Stripe links the
			// rest.
			instrument, agreement := charge.FreezeInstrument(row, chosen.PSPID), charge.AgreementCardOnFile
			if order.HasRecurring() {
				agreement = charge.AgreementRecurring
			}
			if !(order.HasRecurring() && row.Rail == string(models.RailStripe)) {
				if instrument.Mandate, err = mandates.Citable(ctx, q, mid.UUID(), order.CustomerID, methodID, chosen.PSPID, row.Rail, agreement); err != nil {
					return err
				}
			}
			payload.PaymentMethodID, payload.Instrument, charged = methodID, instrument, &methodID
		}
		intent, err = intents.NewStore(d).Enqueue(ctx, intents.EnqueueParams{MerchantID: mid.UUID(), Provider: target.Rail, PspID: chosen.PSPID,
			IntentType: payments.TypeNMISale, Payload: payload, IdempotencyKey: operationKey, NextAttemptAt: now, Origin: intents.OriginUser, OriginReason: "order payment"})
		if err != nil {
			return err
		}
		state, err := json.Marshal(map[string]any{"operation_id": intent.ID.String()})
		if err != nil {
			return err
		}
		expires := now.Add(orderAttemptTTL)
		if current.ExpiresAt.Before(expires) {
			expires = current.ExpiresAt
		}
		if err := q.CreateOrderAttempt(ctx, gen.CreateOrderAttemptParams{ID: attemptID, MerchantID: mid.UUID(), CustomerID: order.CustomerID, OrderID: order.ID,
			Rail: target.Rail, Amount: current.Total, Currency: current.Currency, ExpiresAt: expires, RailState: state, PspID: chosen.PSPID, Now: now}); err != nil {
			return err
		}
		n, err := q.StartOrderAttempt(ctx, gen.StartOrderAttemptParams{MerchantID: mid.UUID(), ID: order.ID, AttemptID: attemptID, PaymentMethodID: charged, PspID: chosen.PSPID, Now: now})
		if err == nil && n != 1 {
			err = orders.ErrNotPayable
		}
		return err
	})
	if err != nil {
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
			return OrderCharge{}, orders.ErrInProgress
		}
		return OrderCharge{}, err
	}
	intent, err = sale.Intents.EnqueueOwnedAndExecute(ctx, saleReplayParams(intent), owns)
	if err != nil {
		return OrderCharge{}, err
	}
	return s.afterOrderCharge(ctx, order, attemptID, intent)
}

// NewCardOption is the PSP a new card's token belongs to: the one named, or
// the only option that takes new cards.
func NewCardOption(options []OrderOption, psp uuid.UUID) (*OrderOption, error) {
	if psp != uuid.Nil {
		for i := range options {
			if options[i].PSPID == psp {
				return &options[i], nil
			}
		}
		return nil, orders.ErrOptionMissing
	}
	switch len(options) {
	case 0:
		return nil, orders.ErrOptionMissing
	case 1:
		return &options[0], nil
	}
	return nil, ErrOrderPSPRequired
}

// prepareNewCard is a new card's instrument, facts and reuse: an NMI token is
// charged as is; a Stripe payment method is read, charged on the customer's
// Stripe customer, and saved by the same PaymentIntent.
func (s *CheckoutAttemptService) prepareNewCard(ctx context.Context, checkout *CheckoutService, sale *CheckoutNMISaleService, in OrderPayInput, option OrderOption) (*newCard, error) {
	token := strings.TrimSpace(in.Card.Token)
	if token == "" || len(token) > 255 || cardguard.ContainsPAN(token) {
		return nil, apperr.Invalidf("payment.token is a PSP's single-use card token").WithParam("payment.token")
	}
	var billingCountry string
	if d := in.Card.Billing; d != nil && d.Address != nil && d.Address.Country != nil {
		billingCountry = *d.Address.Country
	}
	out := &newCard{instrument: charge.FrozenInstrument{PSPID: option.PSPID, Custodian: models.CustodianPSP}}
	if models.Rail(option.Rail) != models.RailStripe {
		out.token, out.billing = token, newCardBilling(in.Card.Billing, in.User)
		out.reuse = mandates.KeepsNewCard(in.Card.Reusable, billingCountry, "")
		return out, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if sale.StripeEngines == nil {
		return nil, errors.New("Stripe card sales are not configured")
	}
	psp := option.PSPID
	service, found, err := sale.StripeEngines.ResolveStripeEngineService(ctx, mid.UUID(), &psp)
	if err != nil {
		return nil, err
	}
	if !found || service == nil {
		return nil, errors.New("Stripe sale account unavailable")
	}
	method, err := service.ReadPaymentMethodCard(ctx, token)
	if err != nil {
		return nil, err
	}
	if method == nil || method.Card == nil {
		return nil, fmt.Errorf("%w: Stripe has no card %s", ErrPaymentMethodStale, token)
	}
	customerRef, err := resolveStripeCustomerWith(db.WithPSPID(ctx, psp), checkout.customerStore(), service, in.User)
	if err != nil {
		return nil, err
	}
	if customerRef == "" {
		return nil, errors.New("Stripe customer mapping unavailable")
	}
	if method.CustomerID != "" && method.CustomerID != customerRef {
		return nil, fmt.Errorf("%w: the card belongs to another customer", ErrPaymentMethodStale)
	}
	if method.CustomerID != "" {
		row, err := s.db.Gen(ctx).GetPaymentMethodByRailMethodRefForPSP(ctx, gen.GetPaymentMethodByRailMethodRefForPSPParams{MerchantID: mid.UUID(), Rail: string(models.RailStripe), PspID: psp, RailMethodRef: token})
		switch {
		case err == nil && row.CustomerID.String() == in.User.ID:
			out.saved = row.ID
			return out, nil
		case err != nil && !db.IsNotFound(err):
			return nil, err
		}
	}
	if billingCountry == "" {
		billingCountry = method.BillingCountry
	}
	out.instrument.RailCustomerRef, out.instrument.RailMethodRef = customerRef, token
	out.card, out.fingerprint = method.Card, method.Fingerprint
	out.reuse = mandates.KeepsNewCard(in.Card.Reusable, billingCountry, method.CardCountry)
	return out, nil
}

// newCardBilling is who a new NMI card bills: the details entered with it,
// the customer's email otherwise.
func newCardBilling(d *billing.BillingDetails, user *UserIdentity) *payments.NewCardBilling {
	out := &payments.NewCardBilling{}
	text := func(v *string) string {
		if v == nil {
			return ""
		}
		return strings.TrimSpace(*v)
	}
	if d != nil {
		out.FirstName, out.LastName = cardholdername.Parts(text(d.Name), "", "")
		out.Email, out.Phone = text(d.Email), text(d.Phone)
		if a := d.Address; a != nil {
			out.Address1, out.Address2, out.City = text(a.Line1), text(a.Line2), text(a.City)
			out.State, out.Zip, out.Country = text(a.State), text(a.PostalCode), text(a.Country)
		}
	}
	if out.Email == "" && user != nil && user.Email != nil {
		out.Email = strings.TrimSpace(*user.Email)
	}
	return out
}

// afterOrderCharge records an unfinished charge on its order: a Stripe
// payment awaiting authentication requires the customer's action, anything
// else in flight is processing. A finished charge settled the order in its
// own transaction.
func (s *CheckoutAttemptService) afterOrderCharge(ctx context.Context, order *orders.Order, attemptID uuid.UUID, intent gen.BillingProviderIntent) (OrderCharge, error) {
	switch intent.Status {
	case intents.StatusSucceeded:
		return OrderCharge{}, nil
	case intents.StatusFailedTerminal:
		var evidence map[string]any
		_ = json.Unmarshal(intent.ResultEvidence, &evidence)
		return OrderCharge{Failed: true, Declined: evidence["declined"] == true}, nil
	}
	status := billing.OrderPaymentProcessing
	if intents.EvidenceString(intent, "stripe_payment_intent_id") != "" && authenticationRequired(intent) {
		status = billing.OrderPaymentRequiresAction
	}
	return OrderCharge{}, s.orders.Pending(ctx, order.ID, attemptID, status)
}

func orderSaleFingerprint(order uuid.UUID, in OrderPayInput, key string) string {
	payment := in.PaymentMethodID.String()
	if in.Card != nil {
		payment = "token:" + strings.TrimSpace(in.Card.Token)
	}
	sum := sha256.Sum256([]byte(order.String() + "\x00" + payment + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

// OrderOperation is the provider intent of an order's attempt.
func (s *CheckoutAttemptService) OrderOperation(ctx context.Context, attemptID uuid.UUID) (gen.BillingProviderIntent, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.BillingProviderIntent{}, err
	}
	attempt, err := s.db.Gen(ctx).GetOrderAttempt(ctx, gen.GetOrderAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
	if err != nil {
		return gen.BillingProviderIntent{}, err
	}
	var state struct {
		OperationID uuid.UUID `json:"operation_id"`
	}
	if err := json.Unmarshal(attempt.RailState, &state); err != nil || state.OperationID == uuid.Nil {
		return gen.BillingProviderIntent{}, errors.New("order attempt has no operation")
	}
	return intents.NewStore(s.db).Get(ctx, state.OperationID)
}

// OrderNextAction is what the customer completes for an order awaiting them:
// Stripe's in-page authentication of the attempt's payment.
func (s *CheckoutAttemptService) OrderNextAction(ctx context.Context, order *orders.Order, principal billingauth.Payer, resolver intents.StripeEngineServiceResolver) (*billing.NextAction, error) {
	if !order.AwaitsCustomer() || order.AttemptID == nil {
		return nil, nil
	}
	checkout, ok := s.checkoutService.(*CheckoutService)
	if !ok || resolver == nil {
		return nil, errors.New("order authentication is unavailable")
	}
	operation, err := s.OrderOperation(ctx, *order.AttemptID)
	if err != nil {
		return nil, err
	}
	auth, err := checkout.StripePaymentAuthentication(ctx, operation.ID, principal, resolver)
	if err != nil || auth.ClientSecret == "" {
		return nil, err
	}
	psp := billing.PSPID(uuidOrNil(operation.PspID))
	return &billing.NextAction{Type: "authenticate", PSPID: &psp, Transactions: []string{},
		Payload: map[string]string{"client_secret": auth.ClientSecret, "payment_intent_id": auth.PaymentIntentID}}, nil
}

// ConfirmOrder reads the provider for the order's live attempt after the
// customer acted: the verifier settles it, or it stays where it is. Failed
// reports an attempt that ended without moving money.
func (s *CheckoutAttemptService) ConfirmOrder(ctx context.Context, order *orders.Order, principal billingauth.Payer) (OrderCharge, error) {
	if order.AttemptID == nil || (!order.AwaitsCustomer() && order.Status != string(billing.OrderProcessing)) {
		return OrderCharge{}, nil
	}
	checkout, ok := s.checkoutService.(*CheckoutService)
	if !ok {
		return OrderCharge{}, errors.New("order confirmation is unavailable")
	}
	operation, err := s.OrderOperation(ctx, *order.AttemptID)
	if err != nil {
		return OrderCharge{}, err
	}
	if operation.Rail == string(models.RailStripe) {
		if _, err := checkout.ConfirmStripePaymentAuthentication(ctx, operation.ID, principal); err != nil {
			return OrderCharge{}, err
		}
	} else if verifier, ok := checkout.Intents.(interface {
		VerifyByID(context.Context, uuid.UUID) (gen.BillingProviderIntent, error)
	}); ok {
		if _, err := verifier.VerifyByID(ctx, operation.ID); err != nil {
			return OrderCharge{}, err
		}
	}
	current, err := intents.NewStore(s.db).Get(ctx, operation.ID)
	if err != nil {
		return OrderCharge{}, err
	}
	return s.afterOrderCharge(ctx, order, *order.AttemptID, current)
}

// orderClosedKey marks the sale of an order canceled or expired while its
// payment awaited the customer.
const orderClosedKey = "order_closed"

// AbandonOrderAttempt closes the provider side of a closed order's attempt
// that awaited the customer: the challenged payment is canceled at the
// provider. A payment that completed first settles the order late instead.
func (s *CheckoutAttemptService) AbandonOrderAttempt(ctx context.Context, attemptID uuid.UUID) error {
	operation, err := s.OrderOperation(ctx, attemptID)
	if err != nil || operation.Status == intents.StatusSucceeded || operation.Status == intents.StatusFailedTerminal {
		return err
	}
	if err := intents.NewStore(s.db).RecordProgress(ctx, operation.ID, map[string]any{orderClosedKey: "true"}); err != nil {
		return err
	}
	checkout, ok := s.checkoutService.(*CheckoutService)
	if !ok {
		return nil
	}
	if verifier, ok := checkout.Intents.(interface {
		VerifyByID(context.Context, uuid.UUID) (gen.BillingProviderIntent, error)
	}); ok {
		_, err = verifier.VerifyByID(ctx, operation.ID)
	}
	return err
}

func uuidOrNil(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

// completeOrder settles an order sale in its completion transaction: the
// order is complete (or its late payment refunded), or open again with the
// decline. saved is the method a new card's charge created, nil otherwise.
func (h *NMISaleIntentHandler) completeOrder(ctx context.Context, d *db.DB, in gen.BillingProviderIntent, p payments.NMISalePayload, customer uuid.UUID, receipt *intents.CollectedReceipt, success bool, evidence map[string]any, outcome intents.Outcome, now time.Time, saved *models.PaymentMethod) error {
	if h.Sale.Orders == nil {
		return errors.New("order settlement is unavailable")
	}
	stripe := in.Rail == string(models.RailStripe)
	// The method this sale charged, once it exists: a new card's only when
	// the charge saved it.
	var method *uuid.UUID
	if !p.NewCard {
		method = &p.PaymentMethodID
	}
	if success {
		metadata := map[string]any{"order_reference": payments.NMISaleOrderReference(in.ID, p.E2ERunID)}
		if stripe {
			metadata["stripe_payment_intent_id"] = receipt.StripeEnginePaymentIntentID()
		}
		storing := receipt.TransactionID()
		if stripe {
			storing = receipt.StripeEnginePaymentIntentID()
		}
		// A saved card's storing one-click charge establishes its card-on-file
		// agreement (a Stripe card has its own from its setup); a new card is
		// kept for one-click buys when the customer keeps it.
		reuse := !p.Recurring && p.Instrument.Mandate == nil && !stripe
		if p.NewCard {
			reuse = p.Reuse
			if saved != nil {
				if err := paymentmethods.NewPaymentMethodRepo(d).Create(ctx, saved); err != nil {
					return err
				}
				if _, err := d.Gen(ctx).SetOrderPaymentMethod(ctx, gen.SetOrderPaymentMethodParams{MerchantID: in.MerchantID, ID: p.OrderID, PaymentMethodID: saved.ID, Now: now}); err != nil {
					return err
				}
				method = &saved.ID
			}
		}
		paid := orders.Charge{PaymentID: p.PaymentID, AttemptID: p.CheckoutAttemptID, PSPID: *in.PspID, Rail: in.Rail,
			TransactionID: receipt.TransactionID(), Amount: p.Amount, Currency: p.Currency, PurchasedAt: p.AcceptedAt,
			TokenType: charge.TokenTypePSPToken, Metadata: metadata, Cites: p.Instrument.Mandate, StoringRef: storing, Reuse: reuse}
		if method != nil {
			paid.PaymentMethodID = *method
		}
		if _, err := h.Sale.Orders.Paid(ctx, d, p.OrderID, paid); err != nil {
			return err
		}
		if err := recordSaleAttempt(ctx, d, in, p, customer, method, attempts.Attempt{Approved: true, TransactionID: receipt.TransactionID(), PaymentID: &p.PaymentID}, now); err != nil {
			return err
		}
		evidence["transaction_id"], evidence["payment_id"] = receipt.TransactionID(), p.PaymentID.String()
		return nil
	}
	for key, value := range outcome.Evidence {
		evidence[key] = value
	}
	if evidence["declined"] == true {
		code := evidenceText(evidence, "localization_id")
		if code == "" {
			code = evidenceText(evidence, "decline_code")
		}
		answer := decline.Evidence{Code: code, AVS: evidenceText(evidence, "avs_response"), CVV: evidenceText(evidence, "cvv_response")}
		if err := recordSaleAttempt(ctx, d, in, p, customer, method, attempts.Attempt{Answer: answer, TransactionID: evidenceText(evidence, "decline_transaction_id")}, now); err != nil {
			return err
		}
	}
	return h.Sale.Orders.Declined(ctx, d, p.OrderID, p.CheckoutAttemptID, orderFailure(in.Rail, evidence))
}

// newCardMethod is the payment method a new card's successful order charge
// saved, read from its holder: the NMI vault the sale created, or the Stripe
// card the PaymentIntent attached. nil when the charge saved nothing.
func (h *NMISaleIntentHandler) newCardMethod(ctx context.Context, in gen.BillingProviderIntent, p payments.NMISalePayload, receipt *intents.CollectedReceipt) (*models.PaymentMethod, error) {
	if !p.SavesCard() || receipt == nil {
		return nil, nil
	}
	customer, err := uuid.Parse(p.UserID)
	if err != nil {
		return nil, err
	}
	psp := *in.PspID
	method := &models.PaymentMethod{ID: p.PaymentMethodID, CustomerID: customer, PspID: &psp, Rail: models.Rail(in.Rail), Custodian: models.CustodianPSP}
	if in.Rail == string(models.RailStripe) {
		if p.Card == nil {
			return nil, errors.New("new Stripe card has no reported facts")
		}
		method.RailCustomerRef, method.RailMethodRef, method.Card, method.Fingerprint = p.Instrument.RailCustomerRef, p.Instrument.RailMethodRef, *p.Card, p.Fingerprint
		return method, nil
	}
	vault := receipt.NMICustomerVaultID()
	client, err := h.Sale.nmiClient(db.WithPSPID(ctx, psp), nmiIntentClientName(p.PSP, in.Rail))
	if err != nil || client == nil {
		return nil, fmt.Errorf("NMI account for the new card's vault: %v", err)
	}
	read, found, err := client.GetCustomer(ctx, vault)
	if err != nil || !found {
		return nil, fmt.Errorf("read the new card's vault %s: found=%v: %v", vault, found, err)
	}
	entry := read.PrimaryBilling()
	if entry == nil {
		return nil, fmt.Errorf("vault %s holds no card", vault)
	}
	method.RailCustomerRef, method.RailMethodRef, method.Card = vault, strings.TrimSpace(entry.ID), paymentmethods.VaultedCard(entry.PaymentDetails)
	if b := p.Billing; b != nil {
		if name := cardholdername.Canonical("", b.FirstName, b.LastName); name != "" {
			method.Metadata = map[string]any{"name_on_card": name}
		}
	}
	return method, nil
}

func evidenceText(evidence map[string]any, key string) string {
	switch v := evidence[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

// orderFailure is the customer-facing reason an order's charge failed.
func orderFailure(rail string, evidence map[string]any) *billing.PaymentFailure {
	reason := billing.DeclineProcessingError
	switch {
	case evidence["declined"] == true:
		code := evidenceText(evidence, "localization_id")
		if code == "" {
			code = evidenceText(evidence, "decline_code")
		}
		if code == "" || code == "0" {
			code = evidenceText(evidence, "response_code")
		}
		reason = decline.Classify(rail, code).Reason
		if decline.ProviderFault(reason) {
			reason = billing.DeclineProcessingError
		}
	case evidence["duplicate_refused"] == true:
		reason = billing.DeclineDuplicateTransaction
	case evidenceText(evidence, "decline_code") == "authentication_required":
		reason = billing.DeclineAuthenticationRequired
	}
	failure := reason.Failure()
	return &failure
}
