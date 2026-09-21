package money

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// prepareUnscheduledCollection gives every NMI transport the same immutable
// money and agreement posture. Custodian-specific authority stays in its adapter.
func prepareUnscheduledCollection(method gen.OpenrailsPaymentMethod, req ChargeRequest, charger charge.Charger) (PreparedCharge, error) {
	currency := normalizeCurrency(req.Currency)
	if err := moneyutil.ValidateCurrency(currency); err != nil {
		return nil, fmt.Errorf("collection: refusing to charge without an established currency: %w", err)
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
			return nil, fmt.Errorf("payment method missing approved unscheduled credential reference")
		}
	default:
		return nil, fmt.Errorf("collection initiation is not established")
	}
	if req.AmountCents <= 0 {
		return nil, fmt.Errorf("amount_cents must be positive")
	}
	rail := normalizeRail(method.Rail)
	description := strings.TrimSpace(req.Description)
	if description == "" {
		description = "OpenRails invoice collection"
	}
	request := charge.Request{
		Instrument: charge.Instrument{
			PaymentMethodID: method.ID,
			Rail:            rail,
			CustomerRef:     strings.TrimSpace(method.RailCustomerRef),
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
			Rail:           rail,
			TransactionID:  res.TransactionID,
			Declined:       res.Declined,
			FailureCode:    res.FailureCode,
			FailureMessage: res.FailureMessage,
		}, nil
	}), nil
}
