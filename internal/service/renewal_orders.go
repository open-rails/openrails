package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// An engine-owned renewal order (origin subscription) is paid by its customer
// with a saved card: a customer-present collection of the subscription's period, which is
// the order's next attempt. Paying with another card makes it the
// subscription's; a card whose brand changed is verified first.

var (
	errRenewalSavedCard   = apperr.Invalidf("a renewal is paid with a saved card: payment.payment_method_id").WithParam("payment")
	errOrderSessionNeeded = apperr.New(http.StatusForbidden, billing.CodeCustomerActionRequired, "Paying a renewal or usage order needs the customer's own session.")
)

// payRenewal pays the open renewal order. replayed: key's collection already
// existed.
func (s *Service) payRenewal(ctx context.Context, actor OrderActor, order *orders.Order, pay orderPayment, key string) (payment.OrderCharge, bool, error) {
	if order.CollectionOwner != string(billing.OrderCollectionEngine) {
		return payment.OrderCharge{}, false, orders.ErrNotPayable
	}
	if pay.method == uuid.Nil {
		return payment.OrderCharge{}, false, errRenewalSavedCard
	}
	if order.SubscriptionID == nil {
		return payment.OrderCharge{}, false, errors.New("renewal order has no subscription")
	}
	switch {
	case order.Status == string(billing.OrderProcessing) || order.AwaitsCustomer():
		return payment.OrderCharge{}, false, orders.ErrInProgress
	case order.Status != string(billing.OrderOpen):
		return payment.OrderCharge{}, false, orders.ErrNotPayable
	}
	rt, err := s.runtime()
	if err != nil {
		return payment.OrderCharge{}, false, err
	}
	customer := actor.Customer.UUID()
	admitKey := orderPayKey(order.ID, key)
	if err := s.reconsentPaying(ctx, customer, pay.method, admitKey); err != nil {
		return payment.OrderCharge{}, false, err
	}
	sub, err := subscriptions.NewSubscriptionRepo(rt.DB).GetByID(ctx, *order.SubscriptionID)
	if err != nil {
		return payment.OrderCharge{}, false, err
	}
	charged, err := subscriptions.PaymentMethodOf(ctx, rt.DB.Gen(ctx), sub)
	if err != nil {
		return payment.OrderCharge{}, false, err
	}
	if charged == nil || *charged != pay.method {
		if err := rt.SubscriptionLifecycleService.UpdateEnginePaymentMethod(ctx, sub.ID, customer, &pay.method, rt.RecurringVerifier(sub.MerchantID, intents.OriginUser)); err != nil {
			return payment.OrderCharge{}, false, err
		}
	}
	accepted, replayed, err := s.moneyService().AdmitCustomerSubscriptionCollection(ctx, sub.ID, customer, admitKey, &pay.method, actor.Principal)
	if err != nil {
		return payment.OrderCharge{}, false, renewalRefusal(err)
	}
	var result gen.BillingProviderIntent
	if accepted.Status == intents.StatusUnknownNeedsVerify {
		result, err = rt.IntentRunner().VerifyByID(ctx, accepted.ID)
	} else {
		result, err = rt.IntentRunner().ExecuteByID(ctx, accepted.ID)
	}
	if err != nil {
		return payment.OrderCharge{}, replayed, err
	}
	if result.Status != intents.StatusFailedTerminal {
		return payment.OrderCharge{}, replayed, nil
	}
	var evidence struct {
		Declined bool `json:"declined"`
	}
	_ = json.Unmarshal(result.ResultEvidence, &evidence)
	return payment.OrderCharge{Failed: true, Declined: evidence.Declined}, replayed, nil
}

// orderPayKey is the collection key of one /pay request on a renewal or
// usage order.
func orderPayKey(order uuid.UUID, key string) string {
	sum := sha256.Sum256([]byte(order.String() + "\x00" + key))
	return "order:" + hex.EncodeToString(sum[:])
}

// renewalRefusal answers a collection the renewal's admission refused.
func renewalRefusal(err error) error {
	switch {
	case errors.Is(err, money.ErrCustomerSessionRequired):
		return errOrderSessionNeeded
	case errors.Is(err, intents.ErrRebillInProgress), errors.Is(err, subscriptions.ErrRenewalHeldByUpgrade):
		return orders.ErrInProgress
	case errors.Is(err, intents.ErrRebillKeyConflict):
		return errOrderKeyReused
	case errors.Is(err, money.ErrEngineMethodUnusable):
		return fmt.Errorf("%w: %w", payment.ErrPaymentMethodStale, err)
	case errors.Is(err, money.ErrEngineCollectionNotDue), errors.Is(err, intents.ErrRebillNotRetryable), errors.Is(err, intents.ErrRebillUnsupported):
		return apperr.New(http.StatusConflict, billing.CodeOrderNotPayable, "The renewal takes no payment now: "+err.Error())
	}
	return err
}

// renewalOptions is how a renewal order is paid: a saved card, on its
// subscription's account.
func (s *Service) renewalOptions(ctx context.Context, order *orders.Order) ([]billing.OrderPaymentOption, error) {
	if order.CollectionOwner != string(billing.OrderCollectionEngine) {
		return nil, nil
	}
	sub, err := subscriptions.NewSubscriptionRepo(s.rt.DB).GetByID(ctx, *order.SubscriptionID)
	if err != nil {
		return nil, err
	}
	return []billing.OrderPaymentOption{{PSPID: billing.PSPID(sub.PspID), Rail: string(sub.Rail), Accepts: []string{"saved_card"}}}, nil
}

// reconsentPaying verifies, with the customer present, the saved card they
// pay with when its agreements wait for their consent. A card that is not
// theirs, or cannot be charged, is left to the payment to refuse.
func (s *Service) reconsentPaying(ctx context.Context, customer, method uuid.UUID, key string) error {
	if method == uuid.Nil {
		return nil
	}
	err := s.reconsentCard(ctx, customer, method, key)
	if errors.Is(err, paymentmethods.ErrPaymentMethodNotFound) || errors.Is(err, ErrPaymentMethodNotUsable) || errors.Is(err, ErrPaymentMethodEditUnsupported) {
		return nil
	}
	return err
}
