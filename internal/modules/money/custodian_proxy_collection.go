package money

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmiproxy"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// CustodianProxyCollectionAdapter collects invoices from custodian-held
// instruments (#795) through the #297 charge seam: a merchant-initiated
// unscheduled CoF charge, detokenized through the custodian's proxy into the
// PSP's own NMI gateway. Same anchor semantics as the NMI adapter — it IS the
// NMI rail (or#879), reached differently.
type CustodianProxyCollectionAdapter struct {
	Charger *nmiproxy.Charger
}

func NewCustodianProxyCollectionAdapter(charger *nmiproxy.Charger) *CustodianProxyCollectionAdapter {
	return &CustodianProxyCollectionAdapter{Charger: charger}
}

func (a *CustodianProxyCollectionAdapter) Prepare(_ context.Context, method gen.OpenrailsPaymentMethod, req ChargeRequest) (PreparedCharge, error) {
	if a == nil || a.Charger == nil {
		return nil, fmt.Errorf("custodian-proxy collection adapter not initialized")
	}
	if strings.TrimSpace(method.RailMethodRef) == "" {
		return nil, fmt.Errorf("custodian-held payment method missing its custodian token reference")
	}
	anchor := strings.TrimSpace(method.StoredCredentialUnscheduledRef)
	posture := charge.UnscheduledMIT(anchor)
	switch req.Initiator {
	case charge.InitiatorCustomer:
		posture = charge.OneTimeReuse(anchor)
		if anchor == "" {
			posture = charge.InitialOneTime()
		}
	case charge.InitiatorMerchant:
		if anchor == "" {
			return nil, fmt.Errorf("custodian-held payment method missing approved unscheduled credential reference")
		}
	default:
		return nil, fmt.Errorf("collection initiation is not established")
	}
	// Parked instrument (#795 B6): the custody-side credential is gone.
	if strings.TrimSpace(method.ParkReason) != "" {
		return nil, fmt.Errorf("custodian-held instrument %s is parked (%s): custodian token unusable; re-collect the card", method.ID, method.ParkReason)
	}
	if req.AmountCents <= 0 {
		return nil, fmt.Errorf("amount_cents must be positive")
	}
	currency := normalizeCurrency(req.Currency)
	if err := moneyutil.ValidateCurrency(currency); err != nil {
		return nil, fmt.Errorf("custodian-proxy collection: refusing to charge without an established currency: %w", err)
	}
	description := strings.TrimSpace(req.Description)
	if description == "" {
		description = "OpenRails invoice collection"
	}
	charger := a.Charger.WithSource(nmiproxy.Source{
		TokenID:        strings.TrimSpace(method.RailMethodRef),
		Via:            method.ChargeVia,
		NetworkTokenID: strings.TrimSpace(method.NetworkTokenID),
	})
	request := charge.Request{
		Instrument: charge.Instrument{
			PaymentMethodID: method.ID,
			Rail:            nmiproxy.Rail,
			MethodRef:       strings.TrimSpace(method.RailMethodRef),
		},
		AmountMinor: req.AmountCents,
		Currency:    currency,
		Description: description,
		OrderRef:    strings.TrimSpace(req.IdempotencyKey),
		Context:     posture,
	}
	return PreparedChargeFunc(func(ctx context.Context) (ChargeResult, error) {
		res, err := charger.Charge(ctx, request)
		if err != nil {
			return ChargeResult{}, err
		}
		return ChargeResult{
			Rail:           nmiproxy.Rail,
			TransactionID:  res.TransactionID,
			Declined:       res.Declined,
			FailureCode:    res.FailureCode,
			FailureMessage: res.FailureMessage,
		}, nil
	}), nil
}
