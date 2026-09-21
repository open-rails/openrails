package intents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const qualifiedEnrollmentKey = "qualified_enrollment"

// NMIEnrollmentReceipt proves the accepted successor schedule. It is distinct
// from a payment receipt and cannot establish a stored-credential agreement.
type NMIEnrollmentReceipt struct{ data enrollmentReceipt }

type enrollmentReceipt struct {
	Binding receiptBinding         `json:"binding"`
	Facts   nmi.EnrollmentEvidence `json:"facts"`
}

func NMIEnrollmentOrder(in gen.OpenrailsRailIntent) string {
	if in.IntentType == subscriptions.TypeInitialMembership {
		p, err := subscriptions.DecodeInitialMembershipPayload(in)
		if err != nil {
			return ""
		}
		return payments.NMISaleOrderReference(in.ID, p.E2ERunID)
	}
	return "upgs-" + in.ID.String()
}

type enrollmentTerms struct {
	Instrument                                             charge.FrozenInstrument
	OldProviderSubscriptionID, PlanID, Currency, StartDate string
	RecurringAmount                                        int64
	PeriodStart, PeriodEnd                                 time.Time
	PlanPayments                                           *int
}

func decodeEnrollmentTerms(in gen.OpenrailsRailIntent) (enrollmentTerms, error) {
	if in.IntentType == subscriptions.TypeInitialMembership {
		p, err := subscriptions.DecodeInitialMembershipPayload(in)
		if err != nil {
			return enrollmentTerms{}, err
		}
		if p.NativeSchedule == nil {
			return enrollmentTerms{}, errors.New("engine membership has no native schedule")
		}

		return enrollmentTerms{Instrument: p.Instrument, PlanID: p.NativeSchedule.PlanID, Currency: p.Terms.Currency, StartDate: p.NativeSchedule.StartDate, RecurringAmount: p.Terms.RecurringAmount, PeriodStart: p.Terms.PeriodStart, PeriodEnd: p.Terms.PeriodStart.Add(time.Duration(p.NativeSchedule.DayFrequency) * 24 * time.Hour), PlanPayments: &p.NativeSchedule.PlanPayments}, nil
	}
	p, err := subscriptions.DecodeNMIUpgradePayload(in)
	return enrollmentTerms{Instrument: p.Instrument, OldProviderSubscriptionID: p.OldProviderSubscriptionID, PlanID: p.PlanID, Currency: p.Currency, StartDate: p.StartDate, RecurringAmount: p.RecurringAmount, PeriodStart: p.PeriodStart, PeriodEnd: p.PeriodEnd}, err
}

func (r NMIEnrollmentReceipt) SubscriptionID() string { return r.data.Facts.Subscription.ID }

func (r NMIEnrollmentReceipt) Validate(in gen.OpenrailsRailIntent) error {
	p, err := decodeEnrollmentTerms(in)
	if err != nil {
		return err
	}
	binding, err := collectionBinding(in)
	if err != nil || binding != r.data.Binding {
		return errors.New("enrollment receipt belongs to another accepted operation")
	}
	f := r.data.Facts
	sub := f.Subscription
	if in.IntentType == subscriptions.TypeInitialMembership && (f.VaultBillingID == "" || (p.Instrument.RailMethodRef != "" && f.VaultBillingID != p.Instrument.RailMethodRef)) {
		return errors.New("initial schedule does not prove its accepted sole vault billing entry")
	}
	if sub.ID == "" || sub.ID == p.OldProviderSubscriptionID || sub.CustomerVaultID != p.Instrument.RailCustomerRef || sub.Plan == nil || sub.Plan.ID != p.PlanID || sub.DelayedCondition != "active" || f.OrderReference != NMIEnrollmentOrder(in) || f.PONumber != NMIEnrollmentOrder(in) {
		return errors.New("enrollment receipt does not identify the accepted successor")
	}
	switch fmt.Sprint(sub.PausedSubscription) {
	case "0", "false":
	default:
		return errors.New("successor schedule is paused or has no qualified pause state")
	}
	// The schedule API has no currency field. Interpret its decimal using the
	// accepted obligation currency, without claiming this is a paid amount.
	minor, err := moneyutil.NativeToRailMinorExact(p.Currency, p.RecurringAmount)
	if err != nil {
		return err
	}
	actual, err := f.ScheduleAmountMinor(p.Currency)
	if err != nil || actual != minor {
		return errors.New("successor schedule has another recurring amount")
	}
	period := p.PeriodEnd.Sub(p.PeriodStart)
	days, err := strconv.Atoi(sub.Plan.DayFrequency)
	if err != nil || period <= 0 || period%(24*time.Hour) != 0 || days != int(period/(24*time.Hour)) || (sub.Plan.MonthFrequency != "" && sub.Plan.MonthFrequency != "0") {
		return errors.New("successor schedule has another billing cadence")
	}
	payments, err := strconv.Atoi(strings.TrimSpace(sub.Plan.PlanPayments))
	if err != nil || payments < 0 || (p.PlanPayments != nil && payments != *p.PlanPayments) {
		return errors.New("successor schedule has no qualified installment count")
	}
	start, err := time.Parse("20060102", p.StartDate)
	if err != nil || f.NextChargeDate != start.Format("2006-01-02") || sub.NextBillingDate != f.NextChargeDate {
		return errors.New("successor schedule has another first billing date")
	}
	return nil
}

func ReadNMIEnrollmentReceipt(ctx context.Context, in gen.OpenrailsRailIntent, resolver NMIClientResolver, reference string) (NMIEnrollmentReceipt, bool, error) {
	binding, err := collectionBinding(in)
	if err != nil {
		return NMIEnrollmentReceipt{}, false, err
	}
	if _, err := decodeEnrollmentTerms(in); err != nil {
		return NMIEnrollmentReceipt{}, false, err
	}
	client, err := resolveReceiptNMIClient(ctx, resolver, in)
	if err != nil {
		return NMIEnrollmentReceipt{}, false, err
	}
	facts, found, err := client.ReadEnrollmentEvidence(ctx, reference)
	if err != nil || !found {
		return NMIEnrollmentReceipt{}, false, err
	}
	if in.IntentType == subscriptions.TypeInitialMembership {
		p, err := subscriptions.DecodeInitialMembershipPayload(in)
		if err != nil {
			return NMIEnrollmentReceipt{}, false, err
		}
		facts.VaultBillingID, err = client.ReadSingleCardVaultBilling(ctx, p.Instrument.RailCustomerRef, p.Instrument.RailMethodRef)
		if err != nil {
			return NMIEnrollmentReceipt{}, false, err
		}
	}
	receipt := NMIEnrollmentReceipt{enrollmentReceipt{binding, facts}}
	if err := receipt.Validate(in); err != nil {
		return NMIEnrollmentReceipt{}, false, err
	}
	return receipt, true, nil
}

func LoadNMIEnrollmentReceipt(in gen.OpenrailsRailIntent) (NMIEnrollmentReceipt, bool, error) {
	var evidence map[string]json.RawMessage
	if len(in.ResultEvidence) == 0 {
		return NMIEnrollmentReceipt{}, false, nil
	}
	if err := json.Unmarshal(in.ResultEvidence, &evidence); err != nil {
		return NMIEnrollmentReceipt{}, false, err
	}
	raw, found := evidence[qualifiedEnrollmentKey]
	if !found {
		return NMIEnrollmentReceipt{}, false, nil
	}
	var receipt NMIEnrollmentReceipt
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt.data); err != nil {
		return receipt, true, err
	}
	return receipt, true, receipt.Validate(in)
}

func (s *Store) RetainNMIEnrollmentReceipt(ctx context.Context, in gen.OpenrailsRailIntent, receipt NMIEnrollmentReceipt) (NMIEnrollmentReceipt, error) {
	if err := receipt.Validate(in); err != nil {
		return NMIEnrollmentReceipt{}, err
	}
	current, err := s.retainQualifiedEvidence(ctx, in, qualifiedEnrollmentKey, receipt.data)
	if err != nil {
		return NMIEnrollmentReceipt{}, err
	}
	loaded, found, err := LoadNMIEnrollmentReceipt(current)
	if err != nil {
		return NMIEnrollmentReceipt{}, err
	}
	if !found {
		return NMIEnrollmentReceipt{}, errors.New("enrollment custody disappeared before completion")
	}
	return loaded, loaded.Validate(in)
}
