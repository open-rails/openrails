package intents

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"strings"
)

// InvoiceCollectionPayload freezes the charge before submission. The amount
// is the invoice's amount_due at enqueue, in native precision and rail minor
// units; replays never recalculate it. Instrument freezes the method's
// account and custody: submission requires the method to still match it, and
// verification and operator resolution judge receipts only against it.
// payment_method_id also pins the method against custody remap while the
// operation is unresolved (or#297).
type InvoiceCollectionPayload struct {
	Initiator           charge.Initiator        `json:"initiator"`
	InvoiceID           uuid.UUID               `json:"invoice_id"`
	CustomerID          uuid.UUID               `json:"customer_id"`
	AttemptID           uuid.UUID               `json:"attempt_id"`
	PaymentMethodID     uuid.UUID               `json:"payment_method_id"`
	Rail                string                  `json:"rail"`
	Instrument          charge.FrozenInstrument `json:"instrument"`
	Currency            string                  `json:"currency"`
	Amount              int64                   `json:"amount"`
	AmountMinor         moneyutil.Cents         `json:"amount_minor"`
	ProviderCustomerRef string                  `json:"provider_customer_ref"`
	Description         string                  `json:"description"`
}

func DecodeInvoiceCollectionPayload(intent gen.OpenrailsRailIntent) (InvoiceCollectionPayload, error) {
	var p InvoiceCollectionPayload
	if len(intent.Payload) == 0 {
		return p, errors.New("invoice collection intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode invoice collection payload: %w", err)
	}
	if p.InvoiceID == uuid.Nil || p.CustomerID == uuid.Nil || p.AttemptID == uuid.Nil || p.PaymentMethodID == uuid.Nil || p.Rail == "" || p.Amount <= 0 || p.AmountMinor <= 0 || p.Currency == "" {
		return p, errors.New("invoice collection payload is incomplete")
	}
	if p.Initiator != charge.InitiatorMerchant && p.Initiator != charge.InitiatorCustomer {
		return p, errors.New("collection initiation is not established")
	}
	if p.Initiator == charge.InitiatorCustomer && (intent.Origin != string(OriginUser) || p.Rail == "stripe" || p.Instrument.CustodianHeld()) {
		return p, errors.New("customer collection has an unsupported authority or rail")
	}
	if err := p.Instrument.Validate(); err != nil {
		return p, fmt.Errorf("invoice collection payload: %w", err)
	}
	if intent.PspID == nil || *intent.PspID != p.Instrument.PSPID {
		return p, errors.New("invoice collection payload's provider account is not the operation's")
	}
	if intent.ID == uuid.Nil || intent.MerchantID == uuid.Nil || intent.IntentType != "invoice_collection" || intent.Rail != p.Rail || intent.CustodianID != nil {
		return p, errors.New("collection operation identity is incomplete or inconsistent")
	}
	if p.Currency != strings.ToUpper(strings.TrimSpace(p.Currency)) {
		return p, errors.New("collection currency must be canonical")
	}
	minor, err := moneyutil.NativeToRailMinor(p.Currency, p.Amount)
	if err != nil || minor != p.AmountMinor {
		return p, errors.New("collection rail amount does not match frozen due")
	}
	if _, err := moneyutil.RailMinorToNative(p.Currency, p.AmountMinor); err != nil {
		return p, err
	}
	if p.Rail == "stripe" && (p.ProviderCustomerRef == "" || p.Instrument.RailMethodRef == "") {
		return p, errors.New("Stripe operation has no frozen customer or payment method")
	}
	return p, nil
}
