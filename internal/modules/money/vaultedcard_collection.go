package money

import (
	"context"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/vaultedcard"
)

// VaultedCardCollectionAdapter collects invoices and top-ups from vaulted_card
// instruments (#795) through the #297 charge seam: a merchant-initiated
// unscheduled CoF charge, detokenized through the BT proxy into the linked NMI
// gateway. Same anchor semantics as the NMI adapter.
type VaultedCardCollectionAdapter struct {
	Charger *vaultedcard.Charger
}

func NewVaultedCardCollectionAdapter(charger *vaultedcard.Charger) *VaultedCardCollectionAdapter {
	return &VaultedCardCollectionAdapter{Charger: charger}
}

func (a *VaultedCardCollectionAdapter) ChargeSavedMethod(ctx context.Context, method gen.OpenrailsPaymentMethod, req ChargeRequest) (ChargeResult, error) {
	if a == nil || a.Charger == nil {
		return ChargeResult{}, fmt.Errorf("vaulted_card collection adapter not initialized")
	}
	if strings.TrimSpace(method.RailMethodRef) == "" {
		return ChargeResult{}, fmt.Errorf("vaulted_card payment method missing BT token reference")
	}
	// Parked instrument (#795 B6): the vault-side credential is gone — fail
	// LOUDLY; re-collection or operator repair is the only way forward.
	if strings.TrimSpace(method.ParkReason) != "" {
		return ChargeResult{}, fmt.Errorf("vaulted_card instrument %s is parked (%s): vault token unusable; re-collect the card", method.ID, method.ParkReason)
	}
	if req.AmountCents <= 0 {
		return ChargeResult{}, fmt.Errorf("amount_cents must be positive")
	}
	currency := normalizeCurrency(req.Currency)
	if currency == "" {
		currency = DefaultCurrency
	}
	description := strings.TrimSpace(req.Description)
	if description == "" {
		description = "OpenRails invoice collection"
	}

	priorRef := strings.TrimSpace(method.StoredCredentialUnscheduledRef)
	if priorRef == "" {
		log.WithContext(ctx).WithFields(log.Fields{
			"payment_method_id": method.ID,
		}).Warn("vaulted_card collection: instrument has no stored-credential reference; charging reference-less MIT and back-filling on success (#297)")
	}

	res, err := a.Charger.WithSource(vaultedcard.Source{
		TokenID:        strings.TrimSpace(method.RailMethodRef),
		Via:            method.ChargeVia,
		NetworkTokenID: strings.TrimSpace(method.NetworkTokenID),
	}).Charge(ctx, charge.Request{
		Instrument: charge.Instrument{
			PaymentMethodID: method.ID,
			Rail:            vaultedcard.Rail,
			MethodRef:       strings.TrimSpace(method.RailMethodRef),
		},
		AmountMinor: req.AmountCents,
		Currency:    currency,
		Description: description,
		OrderRef:    nmiWireOrderRef(req.IdempotencyKey),
		Context:     charge.UnscheduledMIT(priorRef),
	})
	if err != nil {
		return ChargeResult{}, err
	}
	return ChargeResult{
		Rail:                        vaultedcard.Rail,
		TransactionID:               res.TransactionID,
		Declined:                    res.Declined,
		FailureCode:                 res.FailureCode,
		FailureMessage:              res.FailureMessage,
		CapturedStoredCredentialRef: res.CapturedRef,
	}, nil
}
