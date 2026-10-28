package intents

import (
	"crypto/sha256"
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

// TypeOrderCollection is one charge of a usage order: scheduled, a
// merchant's retry or the customer's payment.
const TypeOrderCollection = "order_collection"

// OrderCollectionPayload freezes the charge before submission. The amount is
// what the usage order had due at enqueue, in native precision and rail minor
// units; replays never recalculate it. Instrument freezes the method's
// account and custody: submission requires the method to still match it, and
// verification and operator resolution judge receipts only against it.
// payment_method_id also pins the method against custody remap while the
// operation is unresolved.
type OrderCollectionPayload struct {
	// OrderCaptchaSubjects preserves the initiating HTTP address for worker completion.
	OrderCaptchaSubjects []string `json:"order_captcha_subjects,omitempty"`

	Initiator           charge.Initiator           `json:"initiator"`
	OrderID             uuid.UUID                  `json:"order_id"`
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

func DecodeOrderCollectionPayload(intent gen.BillingProviderIntent) (OrderCollectionPayload, error) {
	var p OrderCollectionPayload
	if len(intent.Payload) == 0 {
		return p, errors.New("order collection intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode order collection payload: %w", err)
	}
	if p.OrderID == uuid.Nil || p.CustomerID == uuid.Nil || p.PaymentID == uuid.Nil || p.PaymentMethodID == uuid.Nil || p.Rail == "" || p.Amount <= 0 || p.AmountMinor <= 0 || p.Currency == "" {
		return p, errors.New("order collection payload is incomplete")
	}
	if p.Initiator != charge.InitiatorMerchant && p.Initiator != charge.InitiatorCustomer {
		return p, errors.New("collection initiation is not established")
	}
	if p.Initiator == charge.InitiatorCustomer && (intent.Origin != string(OriginUser) || intent.Actor == nil || *intent.Actor != p.CustomerID.String() || p.Rail != "nmi" || (p.Instrument.Custodian != models.CustodianPSP && p.Instrument.Custodian != models.CustodianHyperSwitch) || !charge.CustomerPaymentKeyValid(TypeOrderCollection, p.CustomerID, intent.IdempotencyKey)) {
		return p, errors.New("customer collection has an unsupported authority or rail")
	}
	if err := p.Instrument.Validate(); err != nil {
		return p, fmt.Errorf("order collection payload: %w", err)
	}
	if p.Instrument.Custodian == models.CustodianHyperSwitch {
		if p.HyperSwitch == nil || p.HyperSwitch.Validate() != nil || p.Rail != "nmi" {
			return p, errors.New("HyperSwitch collection has no exact accepted custody binding")
		}
	} else if p.HyperSwitch != nil {
		return p, errors.New("non-HyperSwitch collection carries foreign custody policy")
	}
	if intent.PspID == nil || *intent.PspID != p.Instrument.PSPID {
		return p, errors.New("order collection payload's provider account is not the operation's")
	}
	if intent.ID == uuid.Nil || intent.MerchantID == uuid.Nil || intent.IntentType != TypeOrderCollection || intent.Rail != p.Rail || intent.CustodianID != nil {
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

// OrderCollectionRetryKey scopes a merchant retry's opaque key to its order.
func OrderCollectionRetryKey(orderID uuid.UUID, clientKey string) string {
	digest := sha256.Sum256([]byte(orderID.String() + "\x00" + clientKey))
	return fmt.Sprintf("%s:%s:retry:%x", TypeOrderCollection, orderID, digest[:16])
}

func OrderCollectionRetryKeyValid(orderID uuid.UUID, key string) bool {
	if orderID == uuid.Nil {
		return false
	}
	prefix := fmt.Sprintf("%s:%s:retry:", TypeOrderCollection, orderID)
	encoded, ok := strings.CutPrefix(key, prefix)
	if !ok || encoded == "" {
		return false
	}
	decoded, err := hex.DecodeString(encoded)
	return err == nil && len(decoded) == 16 && encoded == strings.ToLower(encoded)
}

// IsOrderCollectionKey reports whether key was made for an order collection.
func IsOrderCollectionKey(key string) bool {
	return strings.HasPrefix(key, TypeOrderCollection+":")
}

// OrderCollectionKeyValid reports whether key is a collection key of the
// payer's order: the customer's own payment or a merchant retry.
func OrderCollectionKeyValid(payer, order uuid.UUID, key string) bool {
	return payer != uuid.Nil && (charge.CustomerPaymentKeyValid(TypeOrderCollection, payer, key) || OrderCollectionRetryKeyValid(order, key))
}
