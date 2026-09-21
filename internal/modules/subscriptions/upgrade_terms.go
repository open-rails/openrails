package subscriptions

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const TypeNMIUpgrade = "nmi_upgrade"

// NMIUpgradePayload freezes the complete commercial decision before either
// provider submission. Replays never recalculate proration or the billing date.
type NMIUpgradePayload struct {
	Instrument                charge.FrozenInstrument `json:"instrument"`
	RequestedPrice            string                  `json:"requested_price"`
	PSP                       string                  `json:"psp"`
	UserID                    string                  `json:"user_id"`
	Email                     string                  `json:"email"`
	OldSubscriptionID         uuid.UUID               `json:"old_subscription_id"`
	OldPriceID                uuid.UUID               `json:"old_price_id"`
	OldProviderSubscriptionID string                  `json:"old_provider_subscription_id"`
	NewSubscriptionID         uuid.UUID               `json:"new_subscription_id"`
	NewPaymentID              uuid.UUID               `json:"new_payment_id"`
	PriceID                   uuid.UUID               `json:"price_id"`
	ProductID                 uuid.UUID               `json:"product_id"`
	ProductName               string                  `json:"product_name"`
	PlanID                    string                  `json:"plan_id"`
	PaymentMethodID           uuid.UUID               `json:"payment_method_id"`
	RecurringAmount           int64                   `json:"recurring_amount,string"`
	ProrationAmount           int64                   `json:"proration_amount,string"`
	Currency                  string                  `json:"currency"`
	PeriodStart               time.Time               `json:"period_start"`
	PeriodEnd                 time.Time               `json:"period_end"`
	StartDate                 string                  `json:"start_date"`
	Entitlements              map[string]*int         `json:"entitlements"`
	Card                      nmi.CardUserData        `json:"card"`
}

// DecodeNMIUpgradePayload is shared by the executor and opaque payment receipt
// boundary. Neither recovery path takes expected money or instruments from a caller.
func DecodeNMIUpgradePayload(in gen.OpenrailsRailIntent) (NMIUpgradePayload, error) {
	var p NMIUpgradePayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return p, err
	}
	customer, err := uuid.Parse(p.UserID)
	if err != nil || customer == uuid.Nil || in.ID == uuid.Nil || in.MerchantID == uuid.Nil ||
		in.IntentType != TypeNMIUpgrade || in.Rail != "nmi" || in.PspID == nil ||
		*in.PspID != p.Instrument.PSPID || in.CustodianID != nil ||
		in.SubscriptionID == nil || *in.SubscriptionID != p.OldSubscriptionID ||
		in.PriceID == nil || *in.PriceID != p.PriceID || p.OldSubscriptionID == uuid.Nil ||
		p.NewSubscriptionID == uuid.Nil || p.NewPaymentID == uuid.Nil || p.OldPriceID == uuid.Nil ||
		p.PriceID == uuid.Nil || p.ProductID == uuid.Nil || p.PaymentMethodID == uuid.Nil ||
		p.OldProviderSubscriptionID == "" || p.PlanID == "" || !p.PeriodEnd.After(p.PeriodStart) ||
		p.Currency != strings.ToUpper(strings.TrimSpace(p.Currency)) || p.ProrationAmount < 0 || p.RecurringAmount < 0 {
		return p, errors.New("upgrade operation contradicts its accepted terms")
	}
	if err := p.Instrument.Validate(); err != nil {
		return p, err
	}
	if p.Instrument.CustodianHeld() || p.Instrument.RailCustomerRef == "" {
		return p, errors.New("provider upgrade requires its accepted PSP-held instrument")
	}
	start, err := time.Parse("20060102", p.StartDate)
	if err != nil || start.Format("20060102") != p.PeriodEnd.UTC().Format("20060102") {
		return p, errors.New("successor first charge must follow the accepted initial period")
	}
	if _, err := moneyutil.NativeToRailMinorExact(p.Currency, p.RecurringAmount); err != nil {
		return p, err
	}
	if _, err := moneyutil.NativeToRailMinorExact(p.Currency, p.ProrationAmount); err != nil {
		return p, err
	}
	return p, nil
}
