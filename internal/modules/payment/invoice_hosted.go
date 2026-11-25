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
	"github.com/open-rails/openrails/internal/modules/invoices"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// An invoice paid on Stripe's hosted page (Stripe Checkout): paying opens an
// attempt and a payment-mode Checkout Session for the invoice's total, and the
// invoice awaits the customer with a redirect_to_url next action. Stripe sends
// them back to return_url. The session's completion (its webhook, or confirm
// reading it) completes the invoice; its expiry or failed payment returns the
// invoice to requires_payment_method. Closing the invoice expires the session.
// One-time lines only: a recurring line needs a saved card.

// InvoiceHosted pays on the hosted page of the PSP named (zero: the only one
// that takes the invoice): Stripe Checkout, returning to ReturnURL, or a CCBill
// FlexForm.
type InvoiceHosted struct {
	PSPID     uuid.UUID
	ReturnURL string
}

// hostedFlow marks an invoice attempt paid on a provider's hosted page.
const hostedFlow = "hosted"

// payInvoiceHosted opens the key's hosted attempt on the invoice; a key already
// used answers where its attempt stands.
func (s *InvoiceAttemptService) payInvoiceHosted(ctx context.Context, in InvoicePayInput) (InvoiceCharge, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return InvoiceCharge{}, err
	}
	invoice := in.Invoice
	attemptID := InvoiceAttemptID(mid.UUID(), invoice.ID, in.Key)
	prior, err := s.db.Gen(ctx).GetInvoiceAttempt(ctx, gen.GetInvoiceAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
	switch {
	case err == nil:
		if flow := railStateText(prior.RailState, "flow"); flow != hostedFlow && flow != ccbillFlow || prior.InvoiceID == nil || *prior.InvoiceID != invoice.ID {
			return InvoiceCharge{}, fmt.Errorf("%w: %w", ErrInvoiceAttemptConflict, billing.ErrIdempotencyKeyReused)
		}
		return InvoiceCharge{}, nil
	case !db.IsNotFound(err):
		return InvoiceCharge{}, err
	}
	if err := s.requireProviderWrites(ctx); err != nil {
		return InvoiceCharge{}, err
	}
	returnURL := strings.TrimSpace(in.Hosted.ReturnURL)
	if returnURL == "" {
		return InvoiceCharge{}, fmt.Errorf("%w: return_url is required", ErrInvoiceAttemptValidation)
	}
	if err := s.validateReturnURLs(ctx, returnURL); err != nil {
		return InvoiceCharge{}, err
	}
	if err := moneyutil.RequireFiatCurrency(invoice.Currency); err != nil {
		return InvoiceCharge{}, fmt.Errorf("%w: %v", ErrInvoiceAttemptValidation, err)
	}
	purchase := s.purchaseService
	if purchase == nil {
		return InvoiceCharge{}, errors.New("hosted invoice payment is unavailable")
	}
	options, err := s.InvoiceOptions(ctx, in.Prices, in.Products)
	if err != nil {
		return InvoiceCharge{}, err
	}
	option, err := HostedOption(options, in.Hosted.PSPID)
	if err != nil {
		return InvoiceCharge{}, err
	}
	if models.Rail(option.Rail) == models.RailCCBill {
		return InvoiceCharge{}, s.payInvoiceCCBill(ctx, in, *option, attemptID, returnURL)
	}
	minor, err := moneyutil.NativeToRailMinorExact(invoice.Currency, invoice.Total)
	if err != nil {
		return InvoiceCharge{}, err
	}
	raw, err := json.Marshal(map[string]any{"flow": hostedFlow, "return_url": returnURL})
	if err != nil {
		return InvoiceCharge{}, err
	}
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := s.db.NewWithPgxTx(tx).Gen(ctx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: invoice.CustomerID}); err != nil {
			return err
		}
		current, err := q.LockInvoice(ctx, gen.LockInvoiceParams{MerchantID: mid.UUID(), ID: invoice.ID})
		if err != nil {
			return err
		}
		now := s.now().UTC().Truncate(time.Microsecond)
		if err := s.releaseLapsedCCBill(ctx, s.db.NewWithPgxTx(tx), &current, now); err != nil {
			return err
		}
		switch {
		case current.Status == string(billing.InvoiceProcessing) || current.PaymentStatus == string(billing.InvoicePaymentRequiresAction):
			return invoices.ErrInProgress
		case current.CollectionOwner != string(billing.InvoiceCollectionEngine):
			return invoices.ErrNotPayable
		case !invoices.Payable(current, now):
			return invoices.ErrNotPayable
		}
		if err := s.invoices.StillBuyable(ctx, q, invoice, now); err != nil {
			return err
		}
		expires := now.Add(invoiceAttemptTTL)
		if current.ExpiresAt != nil && current.ExpiresAt.Before(expires) {
			expires = *current.ExpiresAt
		}
		if err := q.CreateInvoiceAttempt(ctx, gen.CreateInvoiceAttemptParams{ID: attemptID, MerchantID: mid.UUID(), CustomerID: invoice.CustomerID, InvoiceID: invoice.ID,
			Rail: option.Rail, Status: string(models.InvoiceAttemptStatusRequiresAction), Amount: current.Total, Currency: current.Currency,
			ExpiresAt: expires, RailState: raw, PspID: option.PSPID, Now: now}); err != nil {
			return err
		}
		if n, err := q.StartInvoiceAttempt(ctx, gen.StartInvoiceAttemptParams{MerchantID: mid.UUID(), ID: invoice.ID, AttemptID: attemptID, PspID: option.PSPID, Now: now}); err != nil || n != 1 {
			return errors.Join(err, invoices.ErrNotPayable)
		}
		n, err := q.SetInvoicePending(ctx, gen.SetInvoicePendingParams{MerchantID: mid.UUID(), ID: invoice.ID, AttemptID: attemptID, PaymentStatus: string(billing.InvoicePaymentRequiresAction), Now: now})
		if err == nil && n != 1 {
			err = invoices.ErrNotPayable
		}
		return err
	})
	if err != nil {
		return InvoiceCharge{}, err
	}
	session, err := purchase.createStripeCheckoutSession(db.WithPSPID(ctx, option.PSPID), stripeCheckoutParams{
		InlinePrice: stripeCheckoutInlinePrice{Name: invoice.Items(), Currency: invoice.Currency, AmountMinor: moneyutil.Cents(minor)},
		SuccessURL:  returnURL, CancelURL: returnURL, UserID: invoice.CustomerID.String(), CustomerEmail: userEmail(in.User),
		InvoiceAttemptID: billing.InvoiceAttemptID(attemptID).String(), InvoiceID: billing.InvoiceID(invoice.ID).String(), IdempotencyKey: "order:" + attemptID.String(),
	})
	if err != nil {
		// No page to send the customer to: the invoice takes another payment.
		failure := &billing.PaymentFailure{Reason: "processing_error", Message: "The payment page could not be opened. Try again."}
		if derr := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return s.invoices.Declined(ctx, s.db.NewWithPgxTx(tx), invoice.ID, attemptID, failure)
		}); derr != nil {
			return InvoiceCharge{}, errors.Join(err, derr)
		}
		return InvoiceCharge{Failed: true}, nil
	}
	state, err := json.Marshal(map[string]any{"checkout_session_id": session.ID, "checkout_url": session.URL})
	if err != nil {
		return InvoiceCharge{}, err
	}
	_, err = s.db.Gen(ctx).SetInvoiceAttemptStatus(ctx, gen.SetInvoiceAttemptStatusParams{MerchantID: mid.UUID(), ID: attemptID,
		Status: string(models.InvoiceAttemptStatusRequiresAction), RailState: state, Now: s.now().UTC()})
	return InvoiceCharge{}, err
}

// CheckReturnURL refuses a return URL off the merchant's return origins.
func (s *InvoiceAttemptService) CheckReturnURL(ctx context.Context, raw string) error {
	return s.validateReturnURLs(ctx, raw)
}

// HostedOption is the hosted-page PSP that takes the invoice: the one named, or
// the only one.
func HostedOption(options []InvoiceOption, psp uuid.UUID) (*InvoiceOption, error) {
	return pick(options, psp, func(o InvoiceOption) bool { return o.Hosted })
}

// hostedAttempt reports an invoice attempt paid on a hosted page.
func hostedAttempt(attempt gen.BillingInvoiceAttempt) bool {
	return railStateText(attempt.RailState, "flow") == hostedFlow
}

// invoiceHostedAction sends the customer to the attempt's hosted page.
func invoiceHostedAction(attempt gen.BillingInvoiceAttempt) *billing.NextAction {
	link := railStateText(attempt.RailState, "checkout_url")
	if link == "" {
		return nil
	}
	psp := billing.PSPID(attempt.PspID)
	return &billing.NextAction{Type: "redirect_to_url", URL: &link, PSPID: &psp, Transactions: []string{}, Payload: map[string]string{}}
}

// confirmInvoiceHosted reads the attempt's Checkout Session: a paid one
// completes the invoice, a closed one returns it to requires_payment_method.
func (s *InvoiceAttemptService) confirmInvoiceHosted(ctx context.Context, attempt gen.BillingInvoiceAttempt) (InvoiceCharge, error) {
	id := railStateText(attempt.RailState, "checkout_session_id")
	if id == "" {
		return InvoiceCharge{}, nil
	}
	session, err := s.stripeCheckoutSession(db.WithPSPID(ctx, attempt.PspID), id, false)
	if err != nil {
		return InvoiceCharge{}, err
	}
	switch {
	case session.Status == "complete" && (session.PaymentStatus == "paid" || session.PaymentStatus == "no_payment_required"):
		return InvoiceCharge{}, s.SettleInvoiceHosted(ctx, attempt.ID, session.ID, session.PaymentIntent, session.Currency, session.AmountTotal)
	case session.Status == "expired":
		return InvoiceCharge{Failed: true}, s.CloseInvoiceHosted(ctx, attempt.ID, false)
	}
	return InvoiceCharge{}, nil
}

// abandonInvoiceHosted expires the Checkout Session of a closed invoice's
// attempt. A session paid first completes the invoice late instead.
func (s *InvoiceAttemptService) abandonInvoiceHosted(ctx context.Context, attempt gen.BillingInvoiceAttempt) error {
	id := railStateText(attempt.RailState, "checkout_session_id")
	if id == "" {
		return nil
	}
	session, err := s.stripeCheckoutSession(db.WithPSPID(ctx, attempt.PspID), id, true)
	if err != nil {
		return err
	}
	if session.Status == "complete" && session.PaymentStatus == "paid" {
		return s.SettleInvoiceHosted(ctx, attempt.ID, session.ID, session.PaymentIntent, session.Currency, session.AmountTotal)
	}
	return nil
}

// SettleInvoiceHosted completes the invoice a paid Checkout Session pays, in
// one transaction: the payment, completion and fulfilment. A replay of the
// same session answers the completed invoice.
func (s *InvoiceAttemptService) SettleInvoiceHosted(ctx context.Context, attemptID uuid.UUID, sessionID, paymentIntent, currency string, amountMinor int64) error {
	if s.invoices == nil {
		return errors.New("invoice settlement is unavailable")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	attempt, err := s.db.Gen(ctx).GetInvoiceAttempt(ctx, gen.GetInvoiceAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
	if err != nil {
		return err
	}
	if !hostedAttempt(attempt) || attempt.InvoiceID == nil || railStateText(attempt.RailState, "checkout_session_id") != sessionID {
		return fmt.Errorf("checkout session %s does not pay invoice attempt %s", sessionID, attemptID)
	}
	if attempt.Amount == nil || attempt.Currency == nil || !strings.EqualFold(*attempt.Currency, currency) {
		return fmt.Errorf("checkout session %s currency %s is not the invoice's", sessionID, currency)
	}
	minor, err := moneyutil.NativeToRailMinorExact(*attempt.Currency, *attempt.Amount)
	if err != nil {
		return err
	}
	if int64(minor) != amountMinor {
		return fmt.Errorf("checkout session %s amount %d is not the invoice's %d", sessionID, amountMinor, minor)
	}
	transaction := strings.TrimSpace(paymentIntent)
	if transaction == "" {
		transaction = sessionID
	}
	payment := uuidutil.DerivedV7(uuidutil.TimeOf(attemptID), uuid.NameSpaceURL, "openrails:order-payment:"+attemptID.String()+":"+transaction)
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.invoices.Paid(db.WithPSPID(ctx, attempt.PspID), s.db.NewWithPgxTx(tx), *attempt.InvoiceID, invoices.Charge{PaymentID: payment, AttemptID: attemptID, PSPID: attempt.PspID,
			Rail: string(models.RailStripe), TransactionID: transaction, Amount: *attempt.Amount, Currency: *attempt.Currency, PurchasedAt: s.now().UTC(),
			Metadata: map[string]any{"stripe_checkout_session_id": sessionID, "stripe_payment_intent_id": paymentIntent, "invoice_attempt_id": attemptID.String()}})
		return err
	})
}

// CloseInvoiceHosted ends an attempt whose Checkout Session expired (or whose
// delayed payment failed): its invoice takes another payment.
func (s *InvoiceAttemptService) CloseInvoiceHosted(ctx context.Context, attemptID uuid.UUID, failed bool) error {
	if s.invoices == nil {
		return errors.New("invoice settlement is unavailable")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	attempt, err := s.db.Gen(ctx).GetInvoiceAttempt(ctx, gen.GetInvoiceAttemptParams{MerchantID: mid.UUID(), ID: attemptID})
	if err != nil || attempt.InvoiceID == nil || !hostedAttempt(attempt) {
		return err
	}
	failure := &billing.PaymentFailure{Reason: billing.DeclinePaymentPageExpired, Message: "The payment page expired before you paid."}
	if failed {
		failure = &billing.PaymentFailure{Reason: "generic_decline", Message: "The payment was declined."}
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return s.invoices.Declined(ctx, s.db.NewWithPgxTx(tx), *attempt.InvoiceID, attemptID, failure)
	})
}

// stripeCheckoutSession reads (or, with expire, expires) a Checkout Session
// on the context's Stripe PSP.
func (s *InvoiceAttemptService) stripeCheckoutSession(ctx context.Context, id string, expire bool) (stripeCheckoutSession, error) {
	purchase := s.purchaseService
	if purchase == nil || purchase.StripeClients == nil {
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
	resp, err := purchase.StripeClients.Client(0).Do(req)
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
