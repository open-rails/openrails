package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/providerrecovery"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// A usage order (origin usage) bills owed usage in arrears. Its customer
// explicitly pays what it still owes; automatic billing follows its agreement
// and policy. Staff can stop collection, waive the debt or record a remittance.

var (
	errUsageSavedCard      = apperr.Invalidf("a usage order is paid with a saved card: payment.payment_method_id").WithParam("payment")
	errUsageActionRefused  = apperr.New(http.StatusConflict, billing.CodeOrderActionNotAllowed, "The order's origin or status does not allow this action.")
	errUsageNotCollectible = apperr.New(http.StatusConflict, billing.CodeOrderNotPayable, "The usage order takes no collection now.")
	errBillingPaused       = apperr.New(http.StatusServiceUnavailable, billing.CodeServiceUnavailable, "billing is paused while provider recovery completes")
)

// payUsage pays what the open usage order still owes. replayed: key's
// collection already existed.
func (s *Service) payUsage(ctx context.Context, actor OrderActor, order *orders.Order, pay orderPayment, key string) (payment.OrderCharge, bool, error) {
	if pay.method == uuid.Nil {
		return payment.OrderCharge{}, false, errUsageSavedCard
	}
	// The collection decides: a key's earlier collection answers again
	// whatever the order's state is now.
	rt, err := s.runtime()
	if err != nil {
		return payment.OrderCharge{}, false, err
	}
	if rt.MoneyCharger == nil {
		return payment.OrderCharge{}, false, fmt.Errorf("usage collection charger not configured")
	}
	collectKey := orderPayKey(order.ID, key)
	if err := s.reconsentPaying(ctx, actor.Customer.UUID(), pay.method, collectKey); err != nil {
		return payment.OrderCharge{}, false, err
	}
	result, err := s.moneyService().PayUsageNow(ctx, rt.IntentRunner(), actor.Principal, money.UsageCollectionRequest{OrderID: order.ID, IdempotencyKey: collectKey, PaymentMethodID: pay.method})
	if err != nil {
		return payment.OrderCharge{}, false, usageRefusal(err)
	}
	out := collectionCharge(result.Operation)
	if out.Declined && !result.Replayed {
		out.CardDecline = result.Operation.ID
	}
	return out, result.Replayed, nil
}

// collectionCharge is how an order collection ended: failed, and whether
// the card was declined.
func collectionCharge(op gen.BillingProviderIntent) payment.OrderCharge {
	if op.Status != intents.StatusFailedTerminal {
		return payment.OrderCharge{}
	}
	var evidence struct {
		Declined bool `json:"declined"`
	}
	_ = json.Unmarshal(op.ResultEvidence, &evidence)
	return payment.OrderCharge{Failed: true, Declined: evidence.Declined}
}

// usageRefusal answers a collection the usage order refused.
func usageRefusal(err error) error {
	switch {
	case errors.Is(err, money.ErrCustomerSessionRequired):
		return errOrderSessionNeeded
	case errors.Is(err, money.ErrCollectionInProgress), errors.Is(err, money.ErrCollectionOutcomeUnknown):
		return orders.ErrInProgress
	case errors.Is(err, money.ErrCollectionIdempotencyConflict):
		return errOrderKeyReused
	case errors.Is(err, money.ErrUsageNotCollectible):
		return errUsageNotCollectible
	case errors.Is(err, money.ErrUsageActionNotAllowed):
		return errUsageActionRefused
	case errors.Is(err, money.ErrCustomerPaymentUnsupported), errors.Is(err, money.ErrDefaultPaymentMethodInvalid):
		return orders.ErrOptionMissing
	case errors.Is(err, providerrecovery.ErrPending), errors.Is(err, money.ErrUsageRecoveryHeld):
		return errBillingPaused
	}
	return err
}

// usageOptions is how a usage order is paid: a saved card.
func (s *Service) usageOptions(ctx context.Context, order *orders.Order) ([]billing.OrderPaymentOption, error) {
	methods, err := s.rt.DB.Gen(ctx).ListPaymentMethodsByCustomerRails(ctx, gen.ListPaymentMethodsByCustomerRailsParams{MerchantID: order.MerchantID, CustomerID: order.CustomerID, Rails: []string{"nmi"}})
	if err != nil {
		return nil, err
	}
	seen := map[uuid.UUID]bool{}
	out := []billing.OrderPaymentOption{}
	for _, m := range methods {
		if m.PspID == nil || seen[*m.PspID] || m.Status != "active" {
			continue
		}
		seen[*m.PspID] = true
		out = append(out, billing.OrderPaymentOption{PSPID: billing.PSPID(*m.PspID), Rail: m.Rail, Accepts: []string{"saved_card"}})
	}
	return out, nil
}

// VoidMerchantOrder voids an unpaid usage order: what it still owes is
// written off and no longer collected.
func (s *Service) VoidMerchantOrder(ctx context.Context, id billing.OrderID) (*billing.Order, error) {
	return s.usageAction(ctx, id, s.moneyService().VoidUsageOrder)
}

// MarkMerchantOrderUncollectible stops scheduled collection while retaining
// the debt. A customer payment can reopen it.
func (s *Service) MarkMerchantOrderUncollectible(ctx context.Context, id billing.OrderID) (*billing.Order, error) {
	return s.usageAction(ctx, id, s.moneyService().MarkUsageOrderUncollectible)
}

func (s *Service) usageAction(ctx context.Context, id billing.OrderID, act func(context.Context, uuid.UUID) error) (*billing.Order, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := act(ctx, id.UUID()); err != nil {
		switch {
		case errors.Is(err, money.ErrUsageActionNotAllowed):
			return nil, errUsageActionRefused
		case db.IsNotFound(err):
			return nil, orders.ErrNotFound
		}
		return nil, err
	}
	return s.usageOrderView(ctx, id)
}

// FinalizeUsage bills one customer's owed usage of [from, to) in currency
// as a usage order. Idempotent per period.
func (s *Service) FinalizeUsage(ctx context.Context, payer identity.CustomerID, currency string, from, to time.Time) (*billing.Order, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	cur, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	order, err := s.moneyService().FinalizeUsage(ctx, payer, cur, from, to)
	if err != nil {
		return nil, err
	}
	return s.usageOrderView(ctx, billing.OrderID(order.ID))
}

// usageOrderView is an order as the merchant reads it, marked delinquent.
func (s *Service) usageOrderView(ctx context.Context, id billing.OrderID) (*billing.Order, error) {
	rt, err := s.orderRuntime()
	if err != nil {
		return nil, err
	}
	order, err := rt.Orders.Get(ctx, id.UUID())
	if err != nil {
		return nil, err
	}
	view, err := s.merchantOrderView(ctx, order)
	if err != nil {
		return nil, err
	}
	return view, s.markDelinquent(ctx, view)
}
