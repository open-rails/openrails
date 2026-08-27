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

// CustodianProxyCollectionAdapter collects invoices and top-ups from
// custodian-held instruments (#795) through the #297 charge seam: a
// merchant-initiated unscheduled CoF charge, detokenized through the
// custodian's proxy into the PSP's own NMI gateway. Same anchor semantics as
// the NMI adapter — it IS the NMI rail (or#879), reached differently.
type CustodianProxyCollectionAdapter struct {
	Charger *nmiproxy.Charger
}

func NewCustodianProxyCollectionAdapter(charger *nmiproxy.Charger) *CustodianProxyCollectionAdapter {
	return &CustodianProxyCollectionAdapter{Charger: charger}
}

func (a *CustodianProxyCollectionAdapter) ChargeSavedMethod(ctx context.Context, method gen.OpenrailsPaymentMethod, req ChargeRequest) (ChargeResult, error) {
	if a == nil || a.Charger == nil {
		return ChargeResult{}, fmt.Errorf("custodian-proxy collection adapter not initialized")
	}
	if strings.TrimSpace(method.RailMethodRef) == "" {
		return ChargeResult{}, fmt.Errorf("custodian-held payment method missing its custodian token reference")
	}
	// Parked instrument (#795 B6): the custody-side credential is gone — fail
	// LOUDLY; re-collection or operator repair is the only way forward.
	if strings.TrimSpace(method.ParkReason) != "" {
		return ChargeResult{}, fmt.Errorf("custodian-held instrument %s is parked (%s): custodian token unusable; re-collect the card", method.ID, method.ParkReason)
	}
	if req.AmountCents <= 0 {
		return ChargeResult{}, fmt.Errorf("amount_cents must be positive")
	}
	// or#864: NO default — see NMICollectionAdapter. A charge in a guessed
	// currency is the exact failure MONEY/CUR exist to prevent.
	currency := normalizeCurrency(req.Currency)
	if err := moneyutil.ValidateCurrency(currency); err != nil {
		return ChargeResult{}, fmt.Errorf("custodian-proxy collection: refusing to charge without an established currency: %w", err)
	}
	description := strings.TrimSpace(req.Description)
	if description == "" {
		description = "OpenRails invoice collection"
	}

	priorRef := strings.TrimSpace(method.StoredCredentialUnscheduledRef)
	if priorRef == "" {
		priorRef = strings.TrimSpace(method.InitialTransactionID)
	}
	credentialContext := charge.UnscheduledMIT(priorRef)
	if priorRef == "" {
		credentialContext = charge.LegacyUnanchoredUnscheduledMIT()
	}

	res, err := a.Charger.WithSource(nmiproxy.Source{
		TokenID:        strings.TrimSpace(method.RailMethodRef),
		Via:            method.ChargeVia,
		NetworkTokenID: strings.TrimSpace(method.NetworkTokenID),
	}).Charge(ctx, charge.Request{
		Instrument: charge.Instrument{
			PaymentMethodID: method.ID,
			Rail:            nmiproxy.Rail,
			MethodRef:       strings.TrimSpace(method.RailMethodRef),
		},
		AmountMinor: req.AmountCents,
		Currency:    currency,
		Description: description,
		OrderRef:    nmiWireOrderRef(req.IdempotencyKey),
		Context:     credentialContext,
	})
	if err != nil {
		return ChargeResult{}, err
	}
	return ChargeResult{
		Rail:                        nmiproxy.Rail,
		TransactionID:               res.TransactionID,
		Declined:                    res.Declined,
		FailureCode:                 res.FailureCode,
		FailureMessage:              res.FailureMessage,
		CapturedStoredCredentialRef: res.CapturedRef,
	}, nil
}
