package intents

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// TypeInvoiceCollection is one charge of a usage invoice: scheduled, a
// merchant's retry or the customer's payment.
const TypeInvoiceCollection = "order_collection"

// InvoiceCollectionPayload freezes the charge before submission. The amount is
// what the usage invoice had due at enqueue, in native precision and rail minor
// units; replays never recalculate it. Instrument freezes the method's
// account and custody: submission requires the method to still match it, and
// verification and operator resolution judge receipts only against it.
// payment_method_id also pins the method against custody remap while the
// operation is unresolved.
type InvoiceCollectionPayload struct {
	// InvoiceCaptchaSubjects preserves the initiating HTTP address for worker completion.
	InvoiceCaptchaSubjects []string `json:"order_captcha_subjects,omitempty"`

	Initiator           charge.Initiator           `json:"initiator"`
	InvoiceID           uuid.UUID                  `json:"order_id"`
	CustomerID          uuid.UUID                  `json:"customer_id"`
	PaymentID           uuid.UUID                  `json:"payment_id"`
	PaymentMethodID     uuid.UUID                  `json:"payment_method_id"`
	Rail                string                     `json:"rail"`
	Instrument          charge.FrozenInstrument    `json:"instrument"`
	HyperSwitch         *charge.HyperSwitchBinding `json:"hyperswitch,omitempty"`
	Currency            string                     `json:"currency"`
	Amount              int64                      `json:"amount"`
	AmountMinor         moneyutil.Cents            `json:"amount_minor"`
	ProviderCustomerRef string                     `json:"provider_customer_ref"`
	Description         string                     `json:"description"`
}

func DecodeInvoiceCollectionPayload(intent gen.BillingProviderIntent) (InvoiceCollectionPayload, error) {
	var p InvoiceCollectionPayload
	if len(intent.Payload) == 0 {
		return p, errors.New("invoice collection intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode invoice collection payload: %w", err)
	}
	if p.InvoiceID == uuid.Nil || p.CustomerID == uuid.Nil || p.PaymentID == uuid.Nil || p.PaymentMethodID == uuid.Nil || p.Rail == "" || p.Amount <= 0 || p.AmountMinor <= 0 || p.Currency == "" {
		return p, errors.New("invoice collection payload is incomplete")
	}
	if p.Initiator != charge.InitiatorMerchant && p.Initiator != charge.InitiatorCustomer {
		return p, errors.New("collection initiation is not established")
	}
	if p.Initiator == charge.InitiatorCustomer && (intent.Origin != string(OriginUser) || intent.Actor == nil || *intent.Actor != p.CustomerID.String() || p.Rail != "nmi" || (p.Instrument.Custodian != models.CustodianPSP && p.Instrument.Custodian != models.CustodianHyperSwitch) || !charge.CustomerPaymentKeyValid(TypeInvoiceCollection, p.CustomerID, intent.IdempotencyKey)) {
		return p, errors.New("customer collection has an unsupported authority or rail")
	}
	if err := p.Instrument.Validate(); err != nil {
		return p, fmt.Errorf("invoice collection payload: %w", err)
	}
	if p.Instrument.Custodian == models.CustodianHyperSwitch {
		if p.HyperSwitch == nil || p.HyperSwitch.Validate() != nil || p.Rail != "nmi" {
			return p, errors.New("HyperSwitch collection has no exact accepted custody binding")
		}
	} else if p.HyperSwitch != nil {
		return p, errors.New("non-HyperSwitch collection carries foreign custody policy")
	}
	if intent.PspID == nil || *intent.PspID != p.Instrument.PSPID {
		return p, errors.New("invoice collection payload's provider account is not the operation's")
	}
	if intent.ID == uuid.Nil || intent.MerchantID == uuid.Nil || intent.IntentType != TypeInvoiceCollection || intent.Rail != p.Rail || intent.CustodianID != nil {
		return p, errors.New("collection operation identity is incomplete or inconsistent")
	}
	if p.Currency != strings.ToUpper(strings.TrimSpace(p.Currency)) {
		return p, errors.New("collection currency must be canonical")
	}
	if err := moneyutil.RequireFiatCurrency(p.Currency); err != nil {
		return p, err
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

func InvoiceCollectionRetryKeyValid(invoiceID uuid.UUID, key string) bool {
	if invoiceID == uuid.Nil {
		return false
	}
	prefix := fmt.Sprintf("%s:%s:retry:", TypeInvoiceCollection, invoiceID)
	encoded, ok := strings.CutPrefix(key, prefix)
	if !ok || encoded == "" {
		return false
	}
	decoded, err := hex.DecodeString(encoded)
	return err == nil && len(decoded) == 16 && encoded == strings.ToLower(encoded)
}

// IsInvoiceCollectionKey reports whether key was made for an invoice collection.
func IsInvoiceCollectionKey(key string) bool {
	return strings.HasPrefix(key, TypeInvoiceCollection+":")
}

// InvoiceCollectionKeyValid reports whether key is a collection key of the
// payer's invoice: the customer's own payment or a merchant retry.
func InvoiceCollectionKeyValid(payer, invoice uuid.UUID, key string) bool {
	return payer != uuid.Nil && (charge.CustomerPaymentKeyValid(TypeInvoiceCollection, payer, key) || InvoiceCollectionRetryKeyValid(invoice, key))
}
