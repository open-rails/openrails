package subscriptions

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

const TypeInitialMembership = "initial_membership"

type NMIInitialScheduleTerms struct {
	PlanID       string           `json:"plan_id"`
	StartDate    string           `json:"start_date"`
	DayFrequency int              `json:"day_frequency"`
	PlanPayments int              `json:"plan_payments"`
	Card         nmi.CardUserData `json:"card"`
}

// InitialMembershipPayload is one accepted operation with mutually exclusive
// native-schedule and engine-custody legs. Terms and Instrument own payment and
// access authority; CheckoutAttemptID binds only the terminal session projection.
type InitialMembershipPayload struct {
	CheckoutAttemptID      *uuid.UUID                 `json:"checkout_attempt_id,omitempty"`
	Terms                  InitialMembershipTerms     `json:"terms"`
	Instrument             charge.FrozenInstrument    `json:"instrument"`
	RequestFingerprint     string                     `json:"request_fingerprint"`
	CheckoutIdempotencyKey string                     `json:"checkout_idempotency_key"`
	NativeSchedule         *NMIInitialScheduleTerms   `json:"native_schedule,omitempty"`
	HyperSwitch            *charge.HyperSwitchBinding `json:"hyperswitch,omitempty"`
	PSP                    string                     `json:"psp"`
	Email                  string                     `json:"email,omitempty"`
	E2ERunID               string                     `json:"e2e_run_id,omitempty"`
	// RequestedPrice is the price reference a tier upgrade named (an id or
	// price key); a replay may name either it or the canonical price id.
	RequestedPrice string `json:"requested_price,omitempty"`
	// Staff charge a change at the customer's request: merchant-initiated,
	// under the card's recurring agreement.
	Staff *StaffChange `json:"staff,omitempty"`
}

// StaffChange is who on the merchant's staff made a change, and why.
type StaffChange struct {
	Invoker string `json:"invoker"`
	Reason  string `json:"reason"`
}

func (c *StaffChange) valid() bool {
	return c != nil && strings.TrimSpace(c.Invoker) != "" && strings.TrimSpace(c.Reason) != ""
}

// Metadata records the change's staff member and reason on its payment.
func (c *StaffChange) Metadata(into map[string]any) map[string]any {
	if c != nil {
		into["changed_by"], into["staff_invoker"], into["reason"] = "staff", c.Invoker, c.Reason
	}
	return into
}

// Initiator is who initiates the operation's charge.
func (p InitialMembershipPayload) Initiator() charge.Initiator {
	if p.Staff != nil {
		return charge.InitiatorMerchant
	}
	return charge.InitiatorCustomer
}

// TierChangeKeyPrefix scopes an upgrade's client key beside checkout keys.
const TierChangeKeyPrefix = "tier_change:"

// Upgrade reports whether this operation is an engine tier upgrade.
func (p InitialMembershipPayload) Upgrade() bool { return p.Terms.Replaces != nil }

// Change reports whether this operation changes an existing membership: a
// tier upgrade or a seat increase.
func (p InitialMembershipPayload) Change() bool { _, ok := p.Terms.Changed(); return ok }

func (p InitialMembershipPayload) DelayedStart() *time.Time {
	if p.Terms.Pending {
		value := p.Terms.PeriodStart
		return &value
	}
	return nil
}

func DecodeInitialMembershipPayload(in gen.BillingProviderIntent) (InitialMembershipPayload, error) {
	var p InitialMembershipPayload
	decoder := json.NewDecoder(bytes.NewReader(in.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return p, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return p, errors.New("initial membership payload has trailing data")
	}

	if p.CheckoutAttemptID != nil && (*p.CheckoutAttemptID == uuid.Nil || p.Terms.CollectionPolicy != models.CollectionPolicyEngine) {
		return p, errors.New("initial membership has invalid quoted-session binding")
	}
	changed, change := p.Terms.Changed()
	if (p.Upgrade() && p.RequestedPrice == "") || (!change && p.RequestedPrice != "") || (change && (p.CheckoutAttemptID != nil || !strings.HasPrefix(p.CheckoutIdempotencyKey, TierChangeKeyPrefix) || in.SubscriptionID == nil || *in.SubscriptionID != changed)) {
		return p, errors.New("engine change has no exact subscription-change binding")
	}
	if err := p.Terms.Validate(); err != nil {
		return p, err
	}
	if err := p.Instrument.Validate(); err != nil {
		return p, err
	}
	if in.ID == uuid.Nil || in.MerchantID == uuid.Nil || in.IntentType != TypeInitialMembership || in.PspID == nil || *in.PspID != p.Instrument.PSPID || p.Instrument.PSPID != p.Terms.PSPID || in.PriceID == nil || *in.PriceID != p.Terms.PriceID || strings.TrimSpace(p.PSP) == "" || p.CheckoutIdempotencyKey == "" || in.IdempotencyKey != TypeInitialMembership+":"+strings.TrimSpace(p.CheckoutIdempotencyKey) {
		return p, errors.New("initial membership contradicts its canonical accepted scope")
	}
	if len(p.RequestFingerprint) != 64 || p.RequestFingerprint != strings.ToLower(p.RequestFingerprint) {
		return p, errors.New("initial membership request fingerprint is invalid")
	}
	if _, err := hex.DecodeString(p.RequestFingerprint); err != nil {
		return p, err
	}
	if p.Terms.Quantity != nil && in.Rail != string(models.RailNMI) && in.Rail != string(models.RailStripe) {
		return p, errors.New("only an NMI or Stripe engine membership has seats")
	}
	// A staff charge changes an engine membership under the agreement the
	// card already carries.
	if p.Staff != nil && (!p.Staff.valid() || !change || p.Terms.CollectionPolicy != models.CollectionPolicyEngine || p.Instrument.Mandate == nil || in.Origin != "admin") {
		return p, errors.New("staff change requires its staff member, reason and the card's recurring agreement")
	}
	if p.Terms.CollectionPolicy == models.CollectionPolicyEngine {
		if p.NativeSchedule != nil || in.CustodianID != nil || p.Terms.Pending || p.Terms.Amount <= 0 || (p.Terms.Amount != p.Terms.RecurringAmount && !change) || !p.Terms.PeriodStart.Equal(p.Terms.AcceptedAt) {
			return p, errors.New("engine initial membership requires its positive customer charge and permanent custody, without a native schedule")
		}
		return p, charge.ValidateEngineInstrument(in.Rail, p.Instrument, p.HyperSwitch)
	}
	schedule := p.NativeSchedule
	if in.Rail != "nmi" || schedule == nil || p.HyperSwitch != nil || in.CustodianID != nil || p.Instrument.CustodianHeld() || p.Instrument.RailCustomerRef == "" || strings.TrimSpace(schedule.PlanID) == "" || schedule.DayFrequency <= 0 || schedule.PlanPayments < 0 {
		return p, errors.New("native initial membership requires its accepted provider schedule and instrument")
	}
	if int64(schedule.DayFrequency) > math.MaxInt64/int64(24*time.Hour) || p.Terms.PeriodEnd.Sub(p.Terms.PeriodStart) != time.Duration(schedule.DayFrequency)*24*time.Hour || (!p.Terms.Pending && (p.Terms.Amount != p.Terms.RecurringAmount || !p.Terms.PeriodStart.Equal(p.Terms.AcceptedAt))) {
		return p, errors.New("initial period or amount contradicts accepted recurring schedule")
	}
	start, err := time.Parse("20060102", schedule.StartDate)
	if err != nil || !start.After(p.Terms.AcceptedAt) {
		return p, errors.New("native initial membership has no exact future recurring date")
	}
	if p.Terms.Pending {
		if !start.Equal(p.Terms.PeriodStart) {
			return p, errors.New("native schedule contradicts accepted coverage")
		}
	} else if start.Format("20060102") != p.Terms.PeriodEnd.UTC().Format("20060102") {
		return p, errors.New("native first recurring date contradicts accepted initial period")
	}
	return p, nil
}
