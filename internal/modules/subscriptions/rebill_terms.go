package subscriptions

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// TypeManualRebill owns accepted recurring recovery through provider
// preparation, charge, receipt and local completion.
const TypeManualRebill = "manual_rebill"

// ManualRebillPayload freezes one attempt, including the local effects the
// confirmed charge buys. OrderReference is the obligation's, shared by every
// attempt for the period; the idempotency key stays per attempt.
type ManualRebillPayload struct {
	Initiator                charge.Initiator        `json:"initiator"`
	RequestedPaymentMethodID *uuid.UUID              `json:"requested_payment_method_id,omitempty"`
	Renewal                  RenewalTerms            `json:"renewal"`
	PaymentMethodID          uuid.UUID               `json:"payment_method_id"`
	Instrument               charge.FrozenInstrument `json:"instrument"`
	Rail                     string                  `json:"rail"`
	RailSubscriptionID       string                  `json:"rail_subscription_id"`
	OrderReference           string                  `json:"order_reference"`
	Attempt                  int                     `json:"attempt"`
	FailureCount             int                     `json:"failure_count"`
	AmountMinor              moneyutil.Cents         `json:"amount_minor,string"`
}

func ManualRebillIdempotencyKey(subscriptionID uuid.UUID, periodEnd time.Time, rail string, attempt int) string {
	return fmt.Sprintf("%s:%s:%s:%s:attempt-%d", TypeManualRebill, subscriptionID,
		periodEnd.UTC().Format(time.RFC3339Nano), strings.ToLower(strings.TrimSpace(rail)), attempt)
}

// ObligationOrderReference is the NMI order id of one obligation: every
// attempt to pay the period that starts at boundary shares it, so a read by
// order answers "was this period paid?" across attempts.
func ObligationOrderReference(subscriptionID uuid.UUID, boundary time.Time) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("obligation:"+subscriptionID.String()+":"+boundary.UTC().Format(time.RFC3339Nano))).String()
}

func DecodeManualRebillPayload(in gen.OpenrailsRailIntent) (ManualRebillPayload, error) {
	var p ManualRebillPayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return p, fmt.Errorf("decode rebill payload: %w", err)
	}
	if err := p.Renewal.Validate(); err != nil {
		return p, err
	}
	if err := p.Instrument.Validate(); err != nil {
		return p, err
	}
	if in.ID == uuid.Nil || in.MerchantID == uuid.Nil || in.IntentType != TypeManualRebill || in.PriceID == nil || *in.PriceID != p.Renewal.PriceID || in.SubscriptionID == nil || *in.SubscriptionID != p.Renewal.SubscriptionID || in.PspID == nil || *in.PspID != p.Instrument.PSPID || p.Renewal.PSPID != p.Instrument.PSPID || in.CustodianID != nil {
		return p, errors.New("rebill operation identity contradicts accepted terms")
	}
	if p.Rail != "nmi" || p.Rail != in.Rail || p.RailSubscriptionID == "" || p.PaymentMethodID == uuid.Nil || p.Attempt < 0 || p.FailureCount < 0 || p.Instrument.CustodianHeld() || p.Instrument.RailCustomerRef == "" || p.Instrument.RailMethodRef == "" || strings.TrimSpace(p.Instrument.StoredCredentialRecurringRef) == "" {
		return p, errors.New("rebill instrument or recurring agreement is incomplete")
	}
	key := ManualRebillIdempotencyKey(p.Renewal.SubscriptionID, p.Renewal.PeriodStart, p.Rail, p.Attempt)
	switch p.Initiator {
	case charge.InitiatorMerchant:
		if in.Origin != "system" || p.RequestedPaymentMethodID != nil || in.IdempotencyKey != key {
			return p, errors.New("scheduled rebill identity contradicts accepted terms")
		}
	case charge.InitiatorCustomer:
		if in.Origin != "user" || in.Actor == nil || *in.Actor != p.Renewal.CustomerID.String() || !charge.CustomerPaymentKeyValid(TypeManualRebill, p.Renewal.CustomerID, in.IdempotencyKey) || (p.RequestedPaymentMethodID != nil && *p.RequestedPaymentMethodID != p.PaymentMethodID) {
			return p, errors.New("customer rebill identity contradicts accepted terms")
		}
	default:
		return p, errors.New("rebill initiation is not established")
	}
	if p.OrderReference != ObligationOrderReference(p.Renewal.SubscriptionID, p.Renewal.PeriodStart) {
		return p, errors.New("rebill order does not name the accepted period")
	}
	minor, err := moneyutil.NativeToRailMinorExact(p.Renewal.Currency, p.Renewal.Amount)
	if err != nil || minor != p.AmountMinor {
		return p, errors.New("rebill rail amount contradicts accepted price")
	}
	return p, nil
}
