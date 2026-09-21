package subscriptions

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// TypeSubscriptionCollection names the existing fleet's accepted engine renewal.
const TypeSubscriptionCollection = "subscription_collection"

// SubscriptionCollectionPayload is one accepted engine renewal. PreviousPeriodEnd
// fences the old obligation even when the purchased period starts after a gap.
type SubscriptionCollectionPayload struct {
	Renewal           RenewalTerms              `json:"renewal"`
	PreviousPeriodEnd time.Time                 `json:"previous_period_end"`
	AcceptedAt        time.Time                 `json:"accepted_at"`
	PaymentMethodID   uuid.UUID                 `json:"payment_method_id"`
	Instrument        charge.FrozenInstrument   `json:"instrument"`
	HyperSwitch       charge.HyperSwitchBinding `json:"hyperswitch"`
	Attempt           int                       `json:"attempt"`
	FailureCount      int                       `json:"failure_count"`
	AmountMinor       moneyutil.Cents           `json:"amount_minor,string"`
	OrderReference    string                    `json:"order_reference"`
}

func SubscriptionCollectionKey(id uuid.UUID, previousPeriodEnd time.Time, attempt int) string {
	return fmt.Sprintf("%s:%s:%s:attempt-%d", TypeSubscriptionCollection, id, previousPeriodEnd.UTC().Format(time.RFC3339Nano), attempt)
}

// SelectEngineRenewalPeriod preserves an ordinary next period until that whole
// period has been missed. Thereafter one newly accepted attempt buys one period
// from admission. Replays never call this selector again.
func SelectEngineRenewalPeriod(terms RenewalTerms, admittedAt time.Time) (RenewalTerms, error) {
	if err := terms.Validate(); err != nil {
		return terms, err
	}
	if admittedAt.IsZero() || admittedAt.Before(terms.PeriodStart) {
		return terms, errors.New("engine renewal is not due")
	}
	if !terms.PeriodEnd.After(admittedAt) {
		cycle := terms.PeriodEnd.Sub(terms.PeriodStart)
		terms.PeriodStart = admittedAt.UTC().Truncate(time.Microsecond)
		terms.PeriodEnd = terms.PeriodStart.Add(cycle)
	}
	return terms, terms.Validate()
}

func DecodeSubscriptionCollectionPayload(in gen.OpenrailsRailIntent) (SubscriptionCollectionPayload, error) {
	var p SubscriptionCollectionPayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return p, err
	}
	if err := p.Renewal.Validate(); err != nil {
		return p, err
	}
	if err := p.Instrument.Validate(); err != nil {
		return p, err
	}
	if err := p.HyperSwitch.Validate(); err != nil {
		return p, err
	}
	if in.ID == uuid.Nil || in.MerchantID == uuid.Nil || in.IntentType != TypeSubscriptionCollection || in.Rail != "nmi" || in.Origin != "system" || in.SubscriptionID == nil || *in.SubscriptionID != p.Renewal.SubscriptionID || in.PriceID == nil || *in.PriceID != p.Renewal.PriceID || in.PspID == nil || *in.PspID != p.Renewal.PSPID || p.Instrument.PSPID != p.Renewal.PSPID || in.CustodianID == nil || p.Instrument.CustodianID == nil || *in.CustodianID != *p.Instrument.CustodianID {
		return p, errors.New("engine renewal operation contradicts accepted scope")
	}
	if p.Instrument.Custodian != models.CustodianHyperSwitch || strings.TrimSpace(p.Instrument.StoredCredentialRecurringRef) == "" || p.PaymentMethodID == uuid.Nil || p.Attempt < 0 || p.FailureCount < 0 || p.AcceptedAt.IsZero() || p.PreviousPeriodEnd.IsZero() || p.AcceptedAt.Before(p.PreviousPeriodEnd) {
		return p, errors.New("engine renewal lacks recurring custody or admission identity")
	}
	cycle := p.Renewal.PeriodEnd.Sub(p.Renewal.PeriodStart)
	expectedStart := p.PreviousPeriodEnd
	if !p.PreviousPeriodEnd.Add(cycle).After(p.AcceptedAt) {
		expectedStart = p.AcceptedAt
	}
	if !p.Renewal.PeriodStart.Equal(expectedStart) {
		return p, errors.New("engine renewal period is not the accepted ordinary or recovery period")
	}

	key := SubscriptionCollectionKey(p.Renewal.SubscriptionID, p.PreviousPeriodEnd, p.Attempt)
	if in.IdempotencyKey != key || p.OrderReference != RebillOrderReference(key) {
		return p, errors.New("engine renewal key contradicts accepted attempt")
	}
	minor, err := moneyutil.NativeToRailMinorExact(p.Renewal.Currency, p.Renewal.Amount)
	if err != nil || minor != p.AmountMinor {
		return p, errors.New("engine renewal amount contradicts accepted terms")
	}
	return p, nil
}
