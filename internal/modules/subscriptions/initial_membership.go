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
// native-schedule and engine-custody legs. Terms and Instrument are the only
// authorities for identities, money, access and saved-credential references.
type InitialMembershipPayload struct {
	Terms                  InitialMembershipTerms     `json:"terms"`
	Instrument             charge.FrozenInstrument    `json:"instrument"`
	RequestFingerprint     string                     `json:"request_fingerprint"`
	CheckoutIdempotencyKey string                     `json:"checkout_idempotency_key"`
	NativeSchedule         *NMIInitialScheduleTerms   `json:"native_schedule,omitempty"`
	HyperSwitch            *charge.HyperSwitchBinding `json:"hyperswitch,omitempty"`
	PSP                    string                     `json:"psp"`
	Email                  string                     `json:"email,omitempty"`
	E2ERunID               string                     `json:"e2e_run_id,omitempty"`
}

func (p InitialMembershipPayload) DelayedStart() *time.Time {
	if p.Terms.Pending {
		value := p.Terms.PeriodStart
		return &value
	}
	return nil
}

func DecodeInitialMembershipPayload(in gen.OpenrailsRailIntent) (InitialMembershipPayload, error) {
	var p InitialMembershipPayload
	decoder := json.NewDecoder(bytes.NewReader(in.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return p, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return p, errors.New("initial membership payload has trailing data")
	}

	if err := p.Terms.Validate(); err != nil {
		return p, err
	}
	if err := p.Instrument.Validate(); err != nil {
		return p, err
	}
	if in.ID == uuid.Nil || in.MerchantID == uuid.Nil || in.IntentType != TypeInitialMembership || in.Rail != "nmi" || in.PspID == nil || *in.PspID != p.Instrument.PSPID || p.Instrument.PSPID != p.Terms.PSPID || in.PriceID == nil || *in.PriceID != p.Terms.PriceID || strings.TrimSpace(p.PSP) == "" || p.CheckoutIdempotencyKey == "" || in.IdempotencyKey != TypeInitialMembership+":"+strings.TrimSpace(p.CheckoutIdempotencyKey) {
		return p, errors.New("initial membership contradicts its canonical accepted scope")
	}
	if len(p.RequestFingerprint) != 64 || p.RequestFingerprint != strings.ToLower(p.RequestFingerprint) {
		return p, errors.New("initial membership request fingerprint is invalid")
	}
	if _, err := hex.DecodeString(p.RequestFingerprint); err != nil {
		return p, err
	}
	if p.Terms.CollectionPolicy == models.CollectionPolicyEngine {
		if p.NativeSchedule != nil || p.HyperSwitch == nil || p.Instrument.Custodian != models.CustodianHyperSwitch || p.Instrument.CustodianID == nil || in.CustodianID != nil || p.Instrument.RailMethodRef == "" || p.Terms.Pending || p.Terms.Amount <= 0 || p.Terms.Amount != p.Terms.RecurringAmount || !p.Terms.PeriodStart.Equal(p.Terms.AcceptedAt) {
			return p, errors.New("engine initial membership requires its positive customer charge and permanent custody, without a native schedule")
		}
		return p, p.HyperSwitch.Validate()
	}
	schedule := p.NativeSchedule
	if schedule == nil || p.HyperSwitch != nil || in.CustodianID != nil || p.Instrument.CustodianHeld() || p.Instrument.RailCustomerRef == "" || strings.TrimSpace(schedule.PlanID) == "" || schedule.DayFrequency <= 0 || schedule.PlanPayments < 0 {
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
