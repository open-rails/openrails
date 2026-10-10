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

// OrderPayInput pays an order with a saved card. Key is the request's scoped
// Idempotency-Key: one key is one attempt, forever.
type OrderPayInput struct {
	Order           *orders.Order
	PaymentMethodID uuid.UUID
	Key             string
	User            *UserIdentity
	Prices          []*models.Price
	Products        []*models.Product
}

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
// and runs its charge inline. The order reflects the outcome: paid, open
// with last_payment_error, requires_action or processing.
func (s *CheckoutAttemptService) PayOrder(ctx context.Context, in OrderPayInput) error {
	if in.Order == nil || in.User == nil || in.Key == "" || in.PaymentMethodID == uuid.Nil {
		return errors.New("order payment needs its order, customer, key and method")
	}
	if err := s.guardCardAttempt(ctx, in.User); err != nil {
		return err
	}
	declined, err := s.payOrder(ctx, in)
	if declined || err != nil && CardAttemptFailed(nil, err) {
		s.noteCardAttempt(ctx, in.User, &CheckoutAttemptResponse{Status: "failed"}, nil)
	}
	return err
}

func (s *CheckoutAttemptService) payOrder(ctx context.Context, in OrderPayInput) (bool, error) {
	sale := s.saleService()
	if sale == nil || sale.Intents == nil || s.orders == nil {
		return false, errors.New("order payment is unavailable")
	}
	if err := s.requireProviderWrites(ctx); err != nil {
		return false, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	order := in.Order
	attemptID := OrderAttemptID(mid.UUID(), order.ID, in.Key)
	fingerprint := orderSaleFingerprint(order.ID, in.PaymentMethodID, in.Key)
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
			return false, err
		}
		intent, err := sale.Intents.EnqueueOwnedAndExecute(ctx, saleReplayParams(prior), owns)
		if err != nil {
			return false, err
		}
		return s.afterOrderCharge(ctx, order, attemptID, intent)
	case !db.IsNotFound(err):
		return false, err
	}

	method, err := s.paymentMethodService.ValidatePaymentMethodOperation(ctx, in.PaymentMethodID, in.User.ID)
	if err != nil {
		if errors.Is(err, paymentmethods.ErrPaymentMethodNotFound) || errors.Is(err, paymentmethods.ErrPaymentMethodAccessDenied) {
			return false, fmt.Errorf("%w: %w", ErrPaymentMethodStale, err)
		}
		return false, err
	}
	if method.Custodian != models.CustodianPSP || method.ParkReason != "" || (method.Rail != models.RailNMI && method.Rail != models.RailStripe) {
		return false, fmt.Errorf("%w: the payment method cannot pay an order", ErrPaymentMethodStale)
	}
	options, err := s.OrderOptions(ctx, in.Prices, in.Products)
	if err != nil {
		return false, err
	}
	var chosen *OrderOption
	for i, o := range options {
		if rails.SameRail(method.Rail, models.Rail(o.Rail)) && method.ChargeableOn(o.PSPID) {
			chosen = &options[i]
			break
		}
	}
	if chosen == nil {
		return false, orders.ErrOptionMissing
	}
	checkout, ok := s.checkoutService.(*CheckoutService)
	if !ok {
		return false, errors.New("order payment is unavailable")
	}
	target, err := checkout.resolveRailTargetForPSP(ctx, chosen.Rail, chosen.PSPID)
	if err != nil {
		return false, err
	}
	if err := sale.requireArmed(ctx, target); err != nil {
		return false, err
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
		case current.Status == string(billing.OrderRequiresAction) || current.Status == string(billing.OrderProcessing):
			return orders.ErrInProgress
		case current.Status != string(billing.OrderOpen) || !current.ExpiresAt.After(now):
			return orders.ErrNotPayable
		}
		if err := s.orders.StillFree(ctx, q, order, now); err != nil {
			return err
		}
		row, err := q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: mid.UUID(), ID: method.ID})
		if err != nil {
			return err
		}
		// The customer pays in person: a recurring line opens a recurring
		// agreement, a one-time order reuses the card on file; each cites the
		// card's lineage, or this charge stores it. On Stripe an order that
		// starts a subscription stores its own agreement (setup_future_usage),
		// as an enrollment does, and Stripe links the rest.
		instrument, agreement := charge.FreezeInstrument(row, chosen.PSPID), charge.AgreementCardOnFile
		if order.HasRecurring() {
			agreement = charge.AgreementRecurring
		}
		if !(order.HasRecurring() && row.Rail == string(models.RailStripe)) {
			if instrument.Mandate, err = mandates.Citable(ctx, q, mid.UUID(), order.CustomerID, method.ID, chosen.PSPID, row.Rail, agreement); err != nil {
				return err
			}
		}
		payload := payments.NMISalePayload{
			CheckoutAttemptID: attemptID, RequestFingerprint: fingerprint, Provider: target.Rail, PSP: target.PSP,
			Amount: current.Total, ListAmount: current.Total, Currency: current.Currency, Description: "Order " + billing.OrderID(order.ID).String(),
			UserID: order.CustomerID.String(), PaymentMethodID: method.ID, Instrument: instrument,
			PaymentID: uuidutil.NewV7(), AcceptedAt: now, Eligibility: string(EligibilityAllowed), OrderID: order.ID, Recurring: order.HasRecurring(),
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
		n, err := q.StartOrderAttempt(ctx, gen.StartOrderAttemptParams{MerchantID: mid.UUID(), ID: order.ID, AttemptID: attemptID, PaymentMethodID: &method.ID, PspID: chosen.PSPID, Now: now})
		if err == nil && n != 1 {
			err = orders.ErrNotPayable
		}
		return err
	})
	if err != nil {
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
			return false, orders.ErrInProgress
		}
		return false, err
	}
	intent, err = sale.Intents.EnqueueOwnedAndExecute(ctx, saleReplayParams(intent), owns)
	if err != nil {
		return false, err
	}
	return s.afterOrderCharge(ctx, order, attemptID, intent)
}

// afterOrderCharge records an unfinished charge on its order: a Stripe
// payment awaiting authentication is requires_action, anything else in flight
// is processing. A finished charge settled the order in its own transaction.
// It reports a decline.
func (s *CheckoutAttemptService) afterOrderCharge(ctx context.Context, order *orders.Order, attemptID uuid.UUID, intent gen.BillingProviderIntent) (bool, error) {
	switch intent.Status {
	case intents.StatusSucceeded:
		return false, nil
	case intents.StatusFailedTerminal:
		var evidence map[string]any
		_ = json.Unmarshal(intent.ResultEvidence, &evidence)
		return evidence["declined"] == true, nil
	}
	status := billing.OrderProcessing
	if intents.EvidenceString(intent, "stripe_payment_intent_id") != "" && authenticationRequired(intent) {
		status = billing.OrderRequiresAction
	}
	return false, s.orders.Pending(ctx, order.ID, attemptID, status)
}

func orderSaleFingerprint(order, method uuid.UUID, key string) string {
	sum := sha256.Sum256([]byte(order.String() + "\x00" + method.String() + "\x00" + key))
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
	if order.Status != string(billing.OrderRequiresAction) || order.AttemptID == nil {
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
// customer acted: the verifier settles it, or it stays where it is.
func (s *CheckoutAttemptService) ConfirmOrder(ctx context.Context, order *orders.Order, principal billingauth.Payer) error {
	if order.AttemptID == nil || (order.Status != string(billing.OrderRequiresAction) && order.Status != string(billing.OrderProcessing)) {
		return nil
	}
	checkout, ok := s.checkoutService.(*CheckoutService)
	if !ok {
		return errors.New("order confirmation is unavailable")
	}
	operation, err := s.OrderOperation(ctx, *order.AttemptID)
	if err != nil {
		return err
	}
	if operation.Rail == string(models.RailStripe) {
		if _, err := checkout.ConfirmStripePaymentAuthentication(ctx, operation.ID, principal); err != nil {
			return err
		}
	} else if verifier, ok := checkout.Intents.(interface {
		VerifyByID(context.Context, uuid.UUID) (gen.BillingProviderIntent, error)
	}); ok {
		if _, err := verifier.VerifyByID(ctx, operation.ID); err != nil {
			return err
		}
	}
	current, err := intents.NewStore(s.db).Get(ctx, operation.ID)
	if err != nil {
		return err
	}
	_, err = s.afterOrderCharge(ctx, order, *order.AttemptID, current)
	return err
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
// order is paid (or its late payment refunded), or open again with the
// decline.
func (h *NMISaleIntentHandler) completeOrder(ctx context.Context, d *db.DB, in gen.BillingProviderIntent, p payments.NMISalePayload, customer uuid.UUID, receipt *intents.CollectedReceipt, success bool, evidence map[string]any, outcome intents.Outcome, now time.Time) error {
	if h.Sale.Orders == nil {
		return errors.New("order settlement is unavailable")
	}
	stripe := in.Rail == string(models.RailStripe)
	if success {
		metadata := map[string]any{"order_reference": payments.NMISaleOrderReference(in.ID, p.E2ERunID)}
		if stripe {
			metadata["stripe_payment_intent_id"] = receipt.StripeEnginePaymentIntentID()
		}
		storing := receipt.TransactionID()
		if stripe {
			storing = receipt.StripeEnginePaymentIntentID()
		}
		if _, err := h.Sale.Orders.Paid(ctx, d, p.OrderID, orders.Charge{PaymentID: p.PaymentID, AttemptID: p.CheckoutAttemptID, PSPID: *in.PspID, Rail: in.Rail,
			TransactionID: receipt.TransactionID(), Amount: p.Amount, Currency: p.Currency, PaymentMethodID: p.PaymentMethodID, PurchasedAt: p.AcceptedAt,
			TokenType: charge.TokenTypePSPToken, Metadata: metadata, Cites: p.Instrument.Mandate, StoringRef: storing}); err != nil {
			return err
		}
		if err := recordSaleAttempt(ctx, d, in, p, customer, attempts.Attempt{Approved: true, TransactionID: receipt.TransactionID(), PaymentID: &p.PaymentID}, now); err != nil {
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
		if err := recordSaleAttempt(ctx, d, in, p, customer, attempts.Attempt{Answer: answer, TransactionID: evidenceText(evidence, "decline_transaction_id")}, now); err != nil {
			return err
		}
	}
	return h.Sale.Orders.Declined(ctx, d, p.OrderID, p.CheckoutAttemptID, orderFailure(in.Rail, evidence))
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
