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
	"github.com/open-rails/openrails/internal/modules/invoices"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// A usage invoice (origin usage) bills owed usage in arrears. Its customer
// explicitly pays what it still owes; automatic billing follows its agreement
// and policy. Staff can stop collection, waive the debt or record a remittance.

var (
	errUsageSavedCard      = apperr.Invalidf("a usage invoice is paid with a saved card: payment.payment_method_id").WithParam("payment")
	errUsageActionRefused  = apperr.New(http.StatusConflict, billing.CodeInvoiceActionNotAllowed, "The invoice's origin or status does not allow this action.")
	errUsageNotCollectible = apperr.New(http.StatusConflict, billing.CodeInvoiceNotPayable, "The usage invoice takes no collection now.")
	errBillingPaused       = apperr.New(http.StatusServiceUnavailable, billing.CodeServiceUnavailable, "this invoice needs its provider payment history to be verified")
)

// payUsage pays what the open usage invoice still owes. replayed: key's
// collection already existed.
func (s *Service) payUsage(ctx context.Context, actor InvoiceActor, invoice *invoices.Invoice, pay invoicePayment, key string) (payment.InvoiceCharge, bool, error) {
	if pay.method == uuid.Nil {
		return payment.InvoiceCharge{}, false, errUsageSavedCard
	}
	// The collection decides: a key's earlier collection answers again
	// whatever the invoice's state is now.
	rt, err := s.runtime()
	if err != nil {
		return payment.InvoiceCharge{}, false, err
	}
	if rt.MoneyCharger == nil {
		return payment.InvoiceCharge{}, false, fmt.Errorf("usage collection charger not configured")
	}
	collectKey := invoicePayKey(invoice.ID, key)
	if err := s.reconsentPaying(ctx, actor.Customer.UUID(), pay.method, collectKey); err != nil {
		return payment.InvoiceCharge{}, false, err
	}
	result, err := s.moneyService().PayUsageNow(ctx, rt.IntentRunner(), actor.Principal, money.UsageCollectionRequest{InvoiceID: invoice.ID, IdempotencyKey: collectKey, PaymentMethodID: pay.method})
	if err != nil {
		return payment.InvoiceCharge{}, false, usageRefusal(err)
	}
	return collectionCharge(result.Operation), result.Replayed, nil
}

// collectionCharge is how an invoice collection ended: failed, and whether
// the card was declined.
func collectionCharge(op gen.BillingProviderIntent) payment.InvoiceCharge {
	if op.Status != intents.StatusFailedTerminal {
		return payment.InvoiceCharge{}
	}
	var evidence struct {
		Declined bool `json:"declined"`
	}
	_ = json.Unmarshal(op.ResultEvidence, &evidence)
	return payment.InvoiceCharge{Failed: true, Declined: evidence.Declined}
}

// usageRefusal answers a collection the usage invoice refused.
func usageRefusal(err error) error {
	switch {
	case errors.Is(err, money.ErrCustomerSessionRequired):
		return errInvoiceSessionNeeded
	case errors.Is(err, money.ErrCollectionInProgress), errors.Is(err, money.ErrCollectionOutcomeUnknown):
		return invoices.ErrInProgress
	case errors.Is(err, money.ErrCollectionIdempotencyConflict):
		return errInvoiceKeyReused
	case errors.Is(err, money.ErrUsageNotCollectible):
		return errUsageNotCollectible
	case errors.Is(err, money.ErrUsageActionNotAllowed):
		return errUsageActionRefused
	case errors.Is(err, money.ErrCustomerPaymentUnsupported), errors.Is(err, money.ErrDefaultPaymentMethodInvalid):
		return invoices.ErrOptionMissing
	case errors.Is(err, money.ErrUsageRecoveryHeld):
		return errBillingPaused
	}
	return err
}

// usageOptions is how a usage invoice is paid: a saved card.
func (s *Service) usageOptions(ctx context.Context, invoice *invoices.Invoice) ([]billing.InvoicePaymentOption, error) {
	methods, err := s.rt.DB.Gen(ctx).ListPaymentMethodsByCustomerRails(ctx, gen.ListPaymentMethodsByCustomerRailsParams{MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID, Rails: []string{"nmi"}})
	if err != nil {
		return nil, err
	}
	seen := map[uuid.UUID]bool{}
	out := []billing.InvoicePaymentOption{}
	for _, m := range methods {
		if m.PspID == nil || seen[*m.PspID] || m.Status != "active" {
			continue
		}
		seen[*m.PspID] = true
		out = append(out, billing.InvoicePaymentOption{PSPID: billing.PSPID(*m.PspID), Rail: billing.Rail(m.Rail), Accepts: []string{"saved_card"}})
	}
	return out, nil
}

// VoidMerchantInvoice voids an unpaid usage invoice: what it still owes is
// written off and no longer collected.
func (s *Service) VoidMerchantInvoice(ctx context.Context, id billing.InvoiceID) (*billing.Invoice, error) {
	return s.usageAction(ctx, id, s.moneyService().VoidUsageInvoice)
}

// MarkMerchantInvoiceUncollectible stops scheduled collection while retaining
// the debt. A customer payment can reopen it.
func (s *Service) MarkMerchantInvoiceUncollectible(ctx context.Context, id billing.InvoiceID) (*billing.Invoice, error) {
	return s.usageAction(ctx, id, s.moneyService().MarkUsageInvoiceUncollectible)
}

func (s *Service) usageAction(ctx context.Context, id billing.InvoiceID, act func(context.Context, uuid.UUID) error) (*billing.Invoice, error) {
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
			return nil, invoices.ErrNotFound
		}
		return nil, err
	}
	return s.usageInvoiceView(ctx, id)
}

// FinalizeUsage bills one customer's owed usage of [from, to) in currency
// as a usage invoice. Idempotent per period.
func (s *Service) FinalizeUsage(ctx context.Context, payer identity.CustomerID, currency string, from, to time.Time) (*billing.Invoice, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	cur, err := s.registryCurrency(ctx, "currency", currency)
	if err != nil {
		return nil, err
	}
	invoice, err := s.moneyService().FinalizeUsage(ctx, payer, cur, from, to, s.now())
	if err != nil {
		return nil, err
	}
	return s.usageInvoiceView(ctx, billing.InvoiceID(invoice.ID))
}

// usageInvoiceView is an invoice as the merchant reads it, marked delinquent.
func (s *Service) usageInvoiceView(ctx context.Context, id billing.InvoiceID) (*billing.Invoice, error) {
	rt, err := s.invoiceRuntime()
	if err != nil {
		return nil, err
	}
	invoice, err := rt.Invoices.Get(ctx, id.UUID())
	if err != nil {
		return nil, err
	}
	view, err := s.merchantInvoiceView(ctx, invoice)
	if err != nil {
		return nil, err
	}
	return view, s.markDelinquent(ctx, view)
}
