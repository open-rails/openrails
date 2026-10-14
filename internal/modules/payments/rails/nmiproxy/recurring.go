package nmiproxy

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// ValidateRecurringAccount keeps NMI references on their source gateway account
// until cross-account reference handling is confirmed. Vault access alone does
// not establish that a destination can continue an existing recurring payment.
func ValidateRecurringAccount(source, destination uuid.UUID) error {
	if source == uuid.Nil || source != destination {
		return errors.New("NMI recurring reference requires its original gateway account")
	}
	return nil
}

// VerifyRecurring establishes a replacement card's recurring reference while
// the customer is present. It verifies the card without collecting money.
func (c *Charger) VerifyRecurring(ctx context.Context, order string) (string, error) {
	if c == nil || c.BT == nil || c.Source.TokenID == "" || c.Source.TokenIntentID != "" {
		return "", errors.New("recurring verification requires a saved platform card")
	}
	req := charge.Request{Currency: "USD", OrderRef: order, Context: charge.Customer(charge.AgreementRecurring, nil)}
	form, err := paymentForm(req, c.Source, c.Gateway, nil, "validate")
	if err != nil {
		return "", err
	}
	if err := c.Gateway.Posture.RequireArmedFor(ctx, c.Gateway.directPostURL(), c.Gateway.SecurityKey); err != nil {
		return "", err
	}
	res, err := c.BT.ProxyForm(ctx, c.Gateway.directPostURL(), form)
	if err != nil {
		return "", err
	}
	verified, err := nmi.ParseSaleResponse(string(res.Body))
	if err != nil {
		return "", err
	}
	return verified.TransactionID, nil
}

// ChargeInitialRecurring handles a customer retry of an accepted renewal.
func (c *Charger) ChargeInitialRecurring(ctx context.Context, req charge.Request) (charge.Result, *nmi.CustomerVaultError, error) {
	if req.Context.Initiator != charge.InitiatorCustomer {
		return charge.Result{}, nil, errors.Join(charge.ErrNotDispatched, errors.New("recurring customer charge requires customer initiation"))
	}
	return c.chargeRecurring(ctx, req)
}

// ChargeRecurringMIT sends one accepted period. The renewal worker owns retries
// and recovers uncertain outcomes against the original gateway account.
func (c *Charger) ChargeRecurringMIT(ctx context.Context, req charge.Request) (charge.Result, *nmi.CustomerVaultError, error) {
	if req.Context.Initiator != charge.InitiatorMerchant || req.Context.Cites == nil {
		return charge.Result{}, nil, errors.Join(charge.ErrNotDispatched, errors.New("recurring renewal requires its merchant initiated agreement"))
	}
	return c.chargeRecurring(ctx, req)
}

func (c *Charger) chargeRecurring(ctx context.Context, req charge.Request) (charge.Result, *nmi.CustomerVaultError, error) {
	if c == nil || c.BT == nil || c.Source.TokenID == "" || c.Source.TokenIntentID != "" || req.Context.Agreement != charge.AgreementRecurring {
		return charge.Result{}, nil, errors.Join(charge.ErrNotDispatched, errors.New("recurring proxy charge requires a saved card and recurring agreement"))
	}
	if err := moneyutil.RequireFiatCurrency(req.Currency); err != nil {
		return charge.Result{}, nil, errors.Join(charge.ErrNotDispatched, err)
	}
	result, refusal, err := c.proxySale(ctx, req, c.Source, nil)
	if pe, ok := basistheory.IsBTProxyError(err); ok && !basistheory.IsTransportAmbiguous(err) {
		return charge.Result{}, nil, errors.Join(charge.ErrNotDispatched, pe)
	}
	return result, refusal, err
}
