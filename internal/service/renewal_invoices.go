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
	"github.com/open-rails/openrails/internal/modules/invoices"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// An engine-owned renewal invoice (origin subscription) is paid by its customer
// with a saved card: a customer-present collection of the subscription's period, which is
// the invoice's next attempt. Paying with another card makes it the
// subscription's; a card whose brand changed is verified first.

var (
	errRenewalSavedCard     = apperr.Invalidf("a renewal is paid with a saved card: payment.payment_method_id").WithParam("payment")
	errInvoiceSessionNeeded = apperr.New(http.StatusForbidden, billing.CodeCustomerActionRequired, "Paying a renewal or usage invoice needs the customer's own session.")
)

// payRenewal pays the open renewal invoice. replayed: key's collection already
// existed.
func (s *Service) payRenewal(ctx context.Context, actor InvoiceActor, invoice *invoices.Invoice, pay invoicePayment, key string) (payment.InvoiceCharge, bool, error) {
	if invoice.CollectionOwner != string(billing.InvoiceCollectionEngine) {
		return payment.InvoiceCharge{}, false, invoices.ErrNotPayable
	}
	if pay.method == uuid.Nil {
		return payment.InvoiceCharge{}, false, errRenewalSavedCard
	}
	if invoice.SubscriptionID == nil {
		return payment.InvoiceCharge{}, false, errors.New("renewal invoice has no subscription")
	}
	switch {
	case invoice.Status == string(billing.InvoiceProcessing) || invoice.AwaitsCustomer():
		return payment.InvoiceCharge{}, false, invoices.ErrInProgress
	case invoice.Status != string(billing.InvoiceOpen):
		return payment.InvoiceCharge{}, false, invoices.ErrNotPayable
	}
	rt, err := s.runtime()
	if err != nil {
		return payment.InvoiceCharge{}, false, err
	}
	user := &payment.UserIdentity{ID: actor.Customer.String(), ClientIP: actor.ClientIP}
	if err := rt.InvoiceAttemptService.GuardCardAttempt(ctx, user); err != nil {
		return payment.InvoiceCharge{}, false, err
	}
	customer := actor.Customer.UUID()
	admitKey := invoicePayKey(invoice.ID, key)
	if err := s.reconsentPaying(ctx, customer, pay.method, admitKey); err != nil {
		return payment.InvoiceCharge{}, false, err
	}
	sub, err := subscriptions.NewSubscriptionRepo(rt.DB).GetByID(ctx, *invoice.SubscriptionID)
	if err != nil {
		return payment.InvoiceCharge{}, false, err
	}
	charged, err := subscriptions.PaymentMethodOf(ctx, rt.DB.Gen(ctx), sub)
	if err != nil {
		return payment.InvoiceCharge{}, false, err
	}
	if charged == nil || *charged != pay.method {
		if err := rt.SubscriptionLifecycleService.UpdateEnginePaymentMethod(ctx, sub.ID, customer, &pay.method, rt.RecurringVerifier(sub.MerchantID, intents.OriginUser)); err != nil {
			return payment.InvoiceCharge{}, false, err
		}
	}
	accepted, replayed, err := s.moneyService().AdmitCustomerSubscriptionCollection(ctx, sub.ID, customer, admitKey, &pay.method, actor.Principal)
	if err != nil {
		return payment.InvoiceCharge{}, false, renewalRefusal(err)
	}
	var result gen.BillingProviderIntent
	if accepted.Status == intents.StatusUnknownNeedsVerify {
		result, err = rt.IntentRunner().VerifyByID(ctx, accepted.ID)
	} else {
		result, err = rt.IntentRunner().ExecuteFirstByID(ctx, accepted.ID)
	}
	if err != nil {
		return payment.InvoiceCharge{}, replayed, err
	}
	if result.Status != intents.StatusFailedTerminal {
		return payment.InvoiceCharge{}, replayed, nil
	}
	var evidence struct {
		Declined bool `json:"declined"`
	}
	_ = json.Unmarshal(result.ResultEvidence, &evidence)
	if evidence.Declined && !replayed {
		rt.InvoiceAttemptService.NoteCardAttempt(ctx, user)
	}
	return payment.InvoiceCharge{Failed: true, Declined: evidence.Declined}, replayed, nil
}

// invoicePayKey is the collection key of one /pay request on a renewal or
// usage invoice.
func invoicePayKey(invoice uuid.UUID, key string) string {
	sum := sha256.Sum256([]byte(invoice.String() + "\x00" + key))
	return "order:" + hex.EncodeToString(sum[:])
}

// renewalRefusal answers a collection the renewal's admission refused.
func renewalRefusal(err error) error {
	switch {
	case errors.Is(err, money.ErrCustomerSessionRequired):
		return errInvoiceSessionNeeded
	case errors.Is(err, intents.ErrRebillInProgress), errors.Is(err, subscriptions.ErrRenewalHeldByUpgrade):
		return invoices.ErrInProgress
	case errors.Is(err, intents.ErrRebillKeyConflict):
		return errInvoiceKeyReused
	case errors.Is(err, intents.ErrRetryNeedsNewMethod):
		return apperr.New(http.StatusConflict, billing.CodeInvoiceNotPayable, "This card is not charged again for this renewal: pay it with another card.")
	case errors.Is(err, money.ErrEngineMethodUnusable):
		return fmt.Errorf("%w: %w", payment.ErrPaymentMethodStale, err)
	case errors.Is(err, money.ErrEngineCollectionNotDue), errors.Is(err, intents.ErrRebillNotRetryable), errors.Is(err, intents.ErrRebillUnsupported):
		return apperr.New(http.StatusConflict, billing.CodeInvoiceNotPayable, "The renewal takes no payment now: "+err.Error())
	}
	return err
}

// renewalOptions is how a renewal invoice is paid: a saved card, on its
// subscription's account.
func (s *Service) renewalOptions(ctx context.Context, invoice *invoices.Invoice) ([]billing.InvoicePaymentOption, error) {
	if invoice.CollectionOwner != string(billing.InvoiceCollectionEngine) {
		return nil, nil
	}
	sub, err := subscriptions.NewSubscriptionRepo(s.rt.DB).GetByID(ctx, *invoice.SubscriptionID)
	if err != nil {
		return nil, err
	}
	return []billing.InvoicePaymentOption{{PSPID: billing.PSPID(sub.PspID), Rail: billing.Rail(sub.Rail), Accepts: []string{"saved_card"}}}, nil
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
