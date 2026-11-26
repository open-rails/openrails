package subscriptions

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
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
	Staff                     *StaffChange            `json:"staff,omitempty"`
	AccessDurationHours       *int                    `json:"access_duration_hours"`
	ProviderAccessDefault     bool                    `json:"provider_access_default,omitempty"`
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
	RecurringIntervalHours    int                     `json:"recurring_interval_hours"`
	RecurringIntervalMonths   int                     `json:"recurring_interval_months,omitempty"`
	CalendarAnchor            *time.Time              `json:"calendar_anchor,omitempty"`
	RecurringAmount           int64                   `json:"recurring_amount,string"`
	ProrationAmount           int64                   `json:"proration_amount,string"`
	Currency                  string                  `json:"currency"`
	PeriodStart               time.Time               `json:"period_start"`
	PeriodEnd                 time.Time               `json:"period_end"`
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
	if p.Staff != nil && !p.Staff.valid() {
		return p, errors.New("invalid staff change identity")
	}
	if err := validateAccessChoice(p.AccessDurationHours, p.ProviderAccessDefault); err != nil {
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
	if !((p.RecurringIntervalHours > 0 && p.RecurringIntervalHours%24 == 0 && p.RecurringIntervalMonths == 0) ||
		(p.RecurringIntervalHours == 0 && p.RecurringIntervalMonths > 0 && p.RecurringIntervalMonths <= 1200)) {
		return p, errors.New("provider tier change requires its accepted recurring cadence")
	}
	if p.RecurringIntervalMonths > 0 && (p.CalendarAnchor == nil || p.CalendarAnchor.IsZero() || p.CalendarAnchor.After(p.PeriodEnd)) || p.RecurringIntervalMonths == 0 && p.CalendarAnchor != nil {
		return p, errors.New("provider tier change lacks its original calendar anchor")
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
