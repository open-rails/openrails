// Package hyperswitch charges a merchant-owned persistent HyperSwitch card
// through the pinned NMI form proxy. The engine sees only opaque card handles;
// the custodian supplies PAN and expiry from the same vaulted card.
package hyperswitch

import (
	"context"
	"errors"
	"net/url"
	"strings"

	provider "github.com/open-rails/openrails/internal/integrations/hyperswitch"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
)

type Charger struct {
	Client      *provider.Client
	Destination string
	SecurityKey provider.Secret
}

var _ charge.Charger = (*Charger)(nil)

func (c *Charger) Charge(ctx context.Context, req charge.Request) (charge.Result, error) {
	if req.Context.Agreement != charge.AgreementUnscheduled {
		return charge.Result{}, errors.Join(charge.ErrNotDispatched, errors.New("HyperSwitch charge requires an established unscheduled initiation"))
	}
	result, _, err := c.charge(ctx, req)
	return result, err
}

// ChargeInitialRecurring is the concrete transport leg for accepted recurring
// customer enrollment. Its caller must establish verified customer consent and
// retain a qualified payment receipt before granting membership. A structured
// refusal is the original sanitized provider rejection, not proof synthesized
// from Result.Declined. Neither approval nor refusal completes an operation here.
func (c *Charger) ChargeInitialRecurring(ctx context.Context, req charge.Request) (charge.Result, *nmi.CustomerVaultError, error) {
	if !recurringCustomerContext(req.Context) {
		return charge.Result{}, nil, errors.Join(charge.ErrNotDispatched, errors.New("HyperSwitch recurring enrollment requires initial or anchored customer initiation"))
	}
	return c.charge(ctx, req)
}

func recurringCustomerContext(c charge.Context) bool {
	return c.Agreement == charge.AgreementRecurring && c.Initiator == charge.InitiatorCustomer &&
		((c.FirstUse && c.PriorRef == "") || (!c.FirstUse && strings.TrimSpace(c.PriorRef) != "" && strings.TrimSpace(c.PriorRef) == c.PriorRef))
}

func (c *Charger) charge(ctx context.Context, req charge.Request) (charge.Result, *nmi.CustomerVaultError, error) {
	if c == nil || c.Client == nil || c.SecurityKey == "" || c.Destination == "" {
		return charge.Result{}, nil, errors.Join(charge.ErrNotDispatched, errors.New("HyperSwitch NMI charge transport is not configured"))
	}
	form, err := saleForm(req, c.SecurityKey)
	if err != nil {
		return charge.Result{}, nil, errors.Join(charge.ErrNotDispatched, err)
	}
	// The permanent method must still belong to this exact vendor customer
	// and merchant. Capture completion alone is not continuing charge authority.
	method, err := c.Client.GetMethod(ctx, req.Instrument.MethodRef, req.Instrument.CustomerRef)
	if err != nil {
		return charge.Result{}, nil, errors.Join(charge.ErrNotDispatched, err)
	}
	if method.ID != req.Instrument.MethodRef {
		return charge.Result{}, nil, errors.Join(charge.ErrNotDispatched, provider.ErrBinding)
	}
	response, err := c.Client.ProxyNMI(ctx, c.Destination, req.Instrument.MethodRef, form)
	if err != nil {
		if errors.Is(err, provider.ErrNotDispatched) {
			return charge.Result{}, nil, errors.Join(charge.ErrNotDispatched, err)
		}
		var refusal *nmi.CustomerVaultError
		if !errors.As(err, &refusal) {
			return charge.Result{}, nil, err
		}
		if !nmidirect.IsHardDecline(refusal.ResponseCode) {
			return charge.Result{}, nil, provider.ErrUnknown
		}
		code := nmidirect.FailureCode(refusal)
		message := "The payment provider refused the charge"
		return charge.Result{TokenType: charge.TokenTypePANViaProxy, Declined: true, FailureCode: &code, FailureMessage: &message}, refusal, nil
	}
	result := charge.Result{TransactionID: response.TransactionID, TokenType: charge.TokenTypePANViaProxy}
	if req.Context.FirstUse {
		result.CapturedRef = response.TransactionID
	}
	return result, nil, nil
}

func saleForm(req charge.Request, key provider.Secret) (map[string]provider.Secret, error) {
	if req.AmountMinor <= 0 || req.OrderRef == "" || len(req.OrderRef) > 50 || strings.TrimSpace(req.OrderRef) != req.OrderRef {
		return nil, errors.New("HyperSwitch charge requires a positive amount and exact operation reference")
	}
	// Unscheduled invoices and explicitly accepted recurring customer enrollment
	// share the same proxy transport. Recurring merchant renewals remain gated.
	if !recurringCustomerContext(req.Context) && (req.Context.Agreement != charge.AgreementUnscheduled ||
		(req.Context.Initiator != charge.InitiatorCustomer && req.Context.Initiator != charge.InitiatorMerchant) ||
		(req.Context.FirstUse && req.Context.Initiator != charge.InitiatorCustomer)) {
		return nil, errors.New("HyperSwitch charge requires an established unscheduled initiation")
	}
	sc := nmidirect.StoredCredentialFor(req.Context)
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	amount, err := nmi.WireAmount(req.AmountMinor, req.Currency)
	if err != nil {
		return nil, err
	}
	fields := url.Values{
		"type": {"sale"}, "security_key": {string(key)},
		"amount": {amount}, "currency": {req.Currency}, "orderid": {req.OrderRef},
		"ccnumber": {"{{$card_number}}"}, "ccexp": {"{{$card_expiry_mmyy}}"},
	}
	if req.Description != "" {
		fields.Set("order_description", req.Description)
	}
	sc.ApplyToForm(fields)
	form := make(map[string]provider.Secret, len(fields))
	for key, values := range fields {
		form[key] = provider.Secret(values[0])
	}
	return form, nil
}
