package nmidirect

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

// ChargeInitialRecurring executes the direct sale leg of an accepted engine
// enrollment. Its caller owns consent, the durable submission fence, qualified
// receipt custody and atomic membership completion. No remote schedule is made.
func (c *Charger) ChargeInitialRecurring(ctx context.Context, req charge.Request) (charge.Result, *nmi.CustomerVaultError, error) {
	if req.Context.Initiator != charge.InitiatorCustomer {
		return recurringNotDispatched(errors.New("native recurring enrollment requires customer initiation"))
	}
	return c.chargeRecurring(ctx, req)
}

// ChargeRecurringMIT charges one accepted engine period against its recurring
// anchor. It never invokes rebill_subscription and never retries a sale.
func (c *Charger) ChargeRecurringMIT(ctx context.Context, req charge.Request) (charge.Result, *nmi.CustomerVaultError, error) {
	if req.Context.Initiator != charge.InitiatorMerchant || req.Context.FirstUse {
		return recurringNotDispatched(errors.New("native recurring renewal requires anchored merchant initiation"))
	}
	return c.chargeRecurring(ctx, req)
}

func recurringNotDispatched(err error) (charge.Result, *nmi.CustomerVaultError, error) {
	return charge.Result{}, nil, errors.Join(charge.ErrNotDispatched, err)
}

func (c *Charger) chargeRecurring(ctx context.Context, req charge.Request) (charge.Result, *nmi.CustomerVaultError, error) {
	if c == nil || c.Client == nil {
		return recurringNotDispatched(errors.New("native recurring transport unavailable"))
	}
	if req.Instrument.Rail != "nmi" || req.Context.Agreement != charge.AgreementRecurring ||
		strings.TrimSpace(req.Context.PriorRef) != req.Context.PriorRef ||
		req.OrderRef == "" || len(req.OrderRef) > 50 || strings.TrimSpace(req.OrderRef) != req.OrderRef || req.AmountMinor <= 0 {
		return recurringNotDispatched(errors.New("native recurring sale requires exact accepted recurring terms"))
	}
	sc := StoredCredentialFor(req.Context)
	if err := sc.Validate(); err != nil {
		return recurringNotDispatched(err)
	}
	if _, err := nmi.WireAmount(req.AmountMinor, req.Currency); err != nil {
		return recurringNotDispatched(err)
	}
	// Reads can fail before the financial request. Such failures prove this call
	// did not dispatch; after RunSale starts every unclassified error is unknown.
	if err := c.Client.PrepareRecurringSale(ctx, req.Instrument.CustomerRef, req.Instrument.MethodRef); err != nil {
		return recurringNotDispatched(err)
	}
	sale, err := c.Client.RunSale(ctx, nmi.SaleParams{
		CustomerVaultID: req.Instrument.CustomerRef, BillingID: req.Instrument.MethodRef,
		Amount: req.AmountMinor, Currency: req.Currency, OrderID: req.OrderRef,
		OrderDescription: req.Description, StoredCredential: sc, DupSeconds: req.DupSeconds,
	})
	if errors.Is(err, nmi.ErrDuplicateTransaction) && req.Context.Initiator == charge.InitiatorCustomer {
		// A customer-present request refused unprocessed resolves as not
		// executed; the customer sees the refusal and may try again.
		return recurringNotDispatched(err)
	}
	// A merchant-initiated renewal refused as a duplicate stays unknown: the
	// matching card and amount charge may be this period's payment, so a new
	// order is never sent until a provider read settles it.
	if err != nil {
		var refusal *nmi.CustomerVaultError
		if !errors.As(err, &refusal) || !IsHardDecline(refusal.ResponseCode) {
			return charge.Result{}, nil, err
		}
		fields, parseErr := url.ParseQuery(refusal.RawResponse)
		code, codeErr := strconv.Atoi(fields.Get("response_code"))
		if parseErr != nil || codeErr != nil || fields.Get("response") != "2" || code != refusal.ResponseCode || code < 200 || code >= 300 {
			return charge.Result{}, nil, err
		}
		// Expose only the structured refusal. Gateway free text and raw bodies are
		// not durable financial proof and must not escape through engine evidence.
		clean := &nmi.CustomerVaultError{Message: "The payment provider refused the charge", ResponseCode: refusal.ResponseCode, LocalizationID: refusal.LocalizationID, RawResponse: url.Values{"response": {"2"}, "response_code": {strconv.Itoa(code)}}.Encode()}
		failureCode, message := FailureCode(clean), clean.Message
		return charge.Result{TokenType: charge.TokenTypePSPToken, Declined: true, FailureCode: &failureCode, FailureMessage: &message}, clean, nil
	}
	if sale == nil || strings.TrimSpace(sale.TransactionID) == "" || strings.TrimSpace(sale.TransactionID) != sale.TransactionID {
		return charge.Result{}, nil, errors.New("native recurring sale has no exact transaction reference; verification required")
	}
	result := charge.Result{TransactionID: sale.TransactionID, TokenType: charge.TokenTypePSPToken}
	if req.Context.FirstUse {
		result.CapturedRef = result.TransactionID
	}
	return result, nil, nil
}
