package intents

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// ManualRebillPayload freezes one attempt, including the local effects the
// confirmed charge buys. OrderReference identifies this attempt, not every
// attempt in a period; a different attempt's receipt cannot settle this one.
type ManualRebillPayload struct {
	Renewal            subscriptions.RenewalTerms `json:"renewal"`
	PaymentMethodID    uuid.UUID                  `json:"payment_method_id"`
	Instrument         charge.FrozenInstrument    `json:"instrument"`
	Rail               string                     `json:"rail"`
	RailSubscriptionID string                     `json:"rail_subscription_id"`
	OrderReference     string                     `json:"order_reference"`
	Attempt            int                        `json:"attempt"`
	FailureCount       int                        `json:"failure_count"`
	AmountMinor        moneyutil.Cents            `json:"amount_minor,string"`
}

func ManualRebillIdempotencyKey(subscriptionID uuid.UUID, periodEnd time.Time, rail string, attempt int) string {
	return fmt.Sprintf("%s:%s:%s:%s:attempt-%d", TypeManualRebill, subscriptionID,
		periodEnd.UTC().Format(time.RFC3339Nano), strings.ToLower(strings.TrimSpace(rail)), attempt)
}

func rebillOrderReference(key string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
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
	if p.Rail == "" || p.Rail != in.Rail || p.Rail == "stripe" || p.RailSubscriptionID == "" || p.PaymentMethodID == uuid.Nil || p.Attempt < 0 || p.FailureCount < 0 || p.Instrument.CustodianHeld() || p.Instrument.RailCustomerRef == "" || p.Instrument.RailMethodRef == "" || strings.TrimSpace(p.Instrument.StoredCredentialRecurringRef) == "" {
		return p, errors.New("rebill instrument or recurring agreement is incomplete")
	}
	key := ManualRebillIdempotencyKey(p.Renewal.SubscriptionID, p.Renewal.PeriodStart, p.Rail, p.Attempt)
	if in.IdempotencyKey != key || p.OrderReference != rebillOrderReference(key) {
		return p, errors.New("rebill identity does not name the accepted period and attempt")
	}
	minor, err := moneyutil.NativeToRailMinorExact(p.Renewal.Currency, p.Renewal.Amount)
	if err != nil || minor != p.AmountMinor {
		return p, errors.New("rebill rail amount contradicts accepted price")
	}
	return p, nil
}
