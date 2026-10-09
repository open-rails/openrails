package subscriptions

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const TypeNMIUpgrade = "nmi_upgrade"

// NMIUpgradePayload freezes an in-place tier change of an NMI-billed
// subscription. NMI keeps its schedule and next billing date (PeriodEnd); an
// upgrade charges ProrationAmount now and moves the schedule's amount to
// RecurringAmount; a downgrade charges nothing, moves the amount (NMI bills it
// from the next renewal) and schedules the local price for that renewal.
type NMIUpgradePayload struct {
	LegacyEntitlements map[string]*int `json:"legacy_entitlements,omitzero"`
	// AccessEndsAt preserves an already accepted pre-split partial-period upgrade.
	AccessEndsAt              *time.Time              `json:"access_ends_at,omitempty"`
	AccessDurationHours       *int                    `json:"access_duration_hours"`
	Action                    string                  `json:"action"` // upgrade | downgrade
	Instrument                charge.FrozenInstrument `json:"instrument"`
	RequestedPrice            string                  `json:"requested_price"`
	PSP                       string                  `json:"psp"`
	UserID                    string                  `json:"user_id"`
	Email                     string                  `json:"email"`
	OldSubscriptionID         uuid.UUID               `json:"old_subscription_id"`
	OldPriceID                uuid.UUID               `json:"old_price_id"`
	OldProviderSubscriptionID string                  `json:"old_provider_subscription_id"`
	NewPaymentID              uuid.UUID               `json:"new_payment_id"`
	PriceID                   uuid.UUID               `json:"price_id"`
	ProductID                 uuid.UUID               `json:"product_id"`
	ProductName               string                  `json:"product_name"`
	PaymentMethodID           uuid.UUID               `json:"payment_method_id"`
	RecurringAmount           int64                   `json:"recurring_amount,string"`
	ProrationAmount           int64                   `json:"proration_amount,string"`
	Currency                  string                  `json:"currency"`
	PeriodStart               time.Time               `json:"period_start"`
	PeriodEnd                 time.Time               `json:"period_end"`
	// Entitlements is the keys a tier change admitted before product access,
	// kept only as accepted evidence. Access follows ProductID.
	Entitlements json.RawMessage `json:"entitlements,omitempty"`
	// TargetPlanID is the named NMI plan the schedule switches to when it is
	// on a named plan (NMI applies plan_amount only to custom schedules).
	// Empty means the schedule's amount is set directly.
	TargetPlanID string `json:"target_plan_id,omitempty"`
}

// Downgrade reports a period-end change: no charge, schedule only.
func (p NMIUpgradePayload) Downgrade() bool { return p.Action == "downgrade" }

// DecodeNMIUpgradePayload is shared by the executor and opaque payment receipt
// boundary. Neither recovery path takes expected money or instruments from a caller.
func DecodeNMIUpgradePayload(in gen.BillingProviderIntent) (NMIUpgradePayload, error) {
	var p NMIUpgradePayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return p, err
	}
	if p.AccessEndsAt != nil && (!p.AccessEndsAt.Equal(p.PeriodEnd) || p.AccessDurationHours != nil) {
		return p, errors.New("legacy accepted upgrade access contradicts its paid period")
	}
	if err := validateAccessDuration(p.AccessDurationHours); err != nil {
		return p, err
	}
	if err := moneyutil.RequireFiatCurrency(p.Currency); err != nil {
		return p, err
	}
	customer, err := uuid.Parse(p.UserID)
	if err != nil || customer == uuid.Nil || in.ID == uuid.Nil || in.MerchantID == uuid.Nil ||
		in.IntentType != TypeNMIUpgrade || in.Rail != "nmi" || in.PspID == nil ||
		*in.PspID != p.Instrument.PSPID || in.CustodianID != nil ||
		in.SubscriptionID == nil || *in.SubscriptionID != p.OldSubscriptionID ||
		in.PriceID == nil || *in.PriceID != p.PriceID || p.OldSubscriptionID == uuid.Nil ||
		p.NewPaymentID == uuid.Nil || p.OldPriceID == uuid.Nil ||
		p.PriceID == uuid.Nil || p.ProductID == uuid.Nil || p.PaymentMethodID == uuid.Nil ||
		p.OldProviderSubscriptionID == "" || !p.PeriodEnd.After(p.PeriodStart) ||
		(p.Action != "upgrade" && p.Action != "downgrade") || (p.Downgrade() && p.ProrationAmount != 0) ||
		p.Currency != strings.ToUpper(strings.TrimSpace(p.Currency)) || p.ProrationAmount < 0 || p.RecurringAmount <= 0 {
		return p, errors.New("tier change operation contradicts its accepted terms")
	}
	if err := p.Instrument.Validate(); err != nil {
		return p, err
	}
	if p.Instrument.CustodianHeld() || p.Instrument.RailCustomerRef == "" {
		return p, errors.New("provider tier change requires its accepted PSP-held instrument")
	}
	if _, err := moneyutil.NativeToRailMinorExact(p.Currency, p.RecurringAmount); err != nil {
		return p, err
	}
	if _, err := moneyutil.NativeToRailMinorExact(p.Currency, p.ProrationAmount); err != nil {
		return p, err
	}
	return p, nil
}

func (p *NMIUpgradePayload) UnmarshalJSON(data []byte) error {
	type plain NMIUpgradePayload
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if _, _, err := grants.DecodeAcceptedEntitlements(decoded.Entitlements, decoded.LegacyEntitlements); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if _, present := fields["access_duration_hours"]; !present {
		decoded.AccessEndsAt = &decoded.PeriodEnd
	}
	*p = NMIUpgradePayload(decoded)
	return nil
}
