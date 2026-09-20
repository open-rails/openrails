package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

const rebillPreparationKey = "rebill_preparation"

// These are observed provider preconditions, captured once before any setup
// write. Currency is intentionally absent: NMI's subscription read has none.
type rebillPreparation struct {
	Binding         receiptBinding `json:"binding"`
	SubscriptionID  string         `json:"subscription_id"`
	CustomerVaultID string         `json:"customer_vault_id"`
	Amount          string         `json:"amount"`
	NextBillingDate string         `json:"next_billing_date"`
	Plan            nmi.V5Plan     `json:"plan"`
}

func snapshotRebillPreparation(in gen.OpenrailsRailIntent, p ManualRebillPayload, remote nmi.V5Subscription) (rebillPreparation, error) {
	var out rebillPreparation
	if remote.ID != p.RailSubscriptionID || remote.CustomerVaultID != p.Instrument.RailCustomerRef || remote.Plan == nil || strings.TrimSpace(remote.NextBillingDate) == "" || strings.TrimSpace(remote.Plan.ID) == "" {
		return out, errors.New("provider subscription does not match the accepted recurring obligation")
	}
	if strings.TrimSpace(remote.DelayedCondition) != "active" {
		return out, errors.New("provider subscription is not active")
	}
	// This provider boolean arrives as false, "0", or numeric 0. Its text
	// form avoids routing any monetary value through floating-point arithmetic.
	switch fmt.Sprint(remote.PausedSubscription) {
	case "false", "0":
	default:
		return out, errors.New("provider subscription is paused or has no qualified pause state")
	}
	if _, err := nmi.SubscriptionAmountMinor(remote, p.Renewal.Currency); err != nil {
		return out, err
	}
	period := p.Renewal.PeriodEnd.Sub(p.Renewal.PeriodStart)
	dayFrequency, err := strconv.Atoi(strings.TrimSpace(remote.Plan.DayFrequency))
	if err != nil || period <= 0 || period%(24*time.Hour) != 0 || dayFrequency != int(period/(24*time.Hour)) || (remote.Plan.MonthFrequency != "" && remote.Plan.MonthFrequency != "0") {
		return out, errors.New("provider recurring cadence does not match the accepted fixed-day period")
	}
	if remote.NextBillingDate != p.Renewal.PeriodEnd.UTC().Format("2006-01-02") {
		return out, errors.New("provider next billing date does not match the accepted renewal period")
	}
	count, err := strconv.Atoi(strings.TrimSpace(remote.Plan.PlanPayments))
	if err != nil || count < 0 {
		return out, errors.New("provider subscription has no qualified installment count")
	}
	binding, err := collectionBinding(in)
	if err != nil {
		return out, err
	}
	out = rebillPreparation{binding, remote.ID, remote.CustomerVaultID, remote.Amount, remote.NextBillingDate, *remote.Plan}
	return out, nil
}

func loadRebillPreparation(in gen.OpenrailsRailIntent) (rebillPreparation, bool, error) {
	var all map[string]json.RawMessage
	if len(in.ResultEvidence) == 0 {
		return rebillPreparation{}, false, nil
	}
	if err := json.Unmarshal(in.ResultEvidence, &all); err != nil {
		return rebillPreparation{}, false, err
	}
	raw, ok := all[rebillPreparationKey]
	if !ok {
		return rebillPreparation{}, false, nil
	}
	var facts rebillPreparation
	if err := json.Unmarshal(raw, &facts); err != nil {
		return facts, true, err
	}
	binding, err := collectionBinding(in)
	if err != nil {
		return facts, true, err
	}
	if facts.Binding != binding {
		return facts, true, errors.New("provider preparation belongs to another accepted operation")
	}
	p, err := DecodeManualRebillPayload(in)
	if err != nil {
		return facts, true, err
	}
	if _, err := snapshotRebillPreparation(in, p, nmi.V5Subscription{ID: facts.SubscriptionID, CustomerVaultID: facts.CustomerVaultID, Amount: facts.Amount, NextBillingDate: facts.NextBillingDate, Plan: &facts.Plan, DelayedCondition: "active", PausedSubscription: false}); err != nil {
		return facts, true, err
	}
	return facts, true, nil
}

func (h *ManualRebillHandler) prepareProvider(ctx context.Context, in gen.OpenrailsRailIntent, p ManualRebillPayload, client *nmi.NMIClient) error {
	accountMerchant, accountPSP := client.AccountIdentity()
	if accountMerchant != in.MerchantID || accountPSP != *in.PspID {
		return errors.New("rebill client is armed for another provider account")
	}
	remote, found, err := client.GetSubscription(ctx, p.RailSubscriptionID)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("provider recurring obligation is not visible")
	}
	observed, err := snapshotRebillPreparation(in, p, remote)
	if err != nil {
		return err
	}
	frozen, retained, err := loadRebillPreparation(in)
	if err != nil {
		return err
	}
	if !retained {
		raw, err := json.Marshal(observed)
		if err != nil {
			return err
		}
		writeCtx, cancel := LedgerWriteContext(ctx)
		defer cancel()
		_, err = h.DB.Gen(writeCtx).RetainRailIntentRebillPreparation(writeCtx, gen.RetainRailIntentRebillPreparationParams{ID: in.ID, MerchantID: in.MerchantID, PspID: *in.PspID, Payload: in.Payload, Preparation: raw})
		if err != nil {
			return err
		}
		current, err := NewStore(h.DB).Get(writeCtx, in.ID)
		if err != nil {
			return err
		}
		frozen, retained, err = loadRebillPreparation(current)
		if err != nil {
			return err
		}
		if !retained {
			return errors.New("provider preparation was not retained")
		}
	}
	// Amount is the only field preparation may deliberately change. All
	// identifying, schedule and finite-installment facts remain the first read.
	comparison := observed
	comparison.Amount, comparison.Plan.PlanAmount = frozen.Amount, frozen.Plan.PlanAmount
	if !reflect.DeepEqual(comparison, frozen) {
		return errors.New("provider recurring preconditions changed after acceptance")
	}
	actual, err := nmi.SubscriptionAmountMinor(remote, p.Renewal.Currency)
	if err != nil {
		return err
	}
	if actual == p.AmountMinor {
		return nil
	}
	if p.Renewal.PriceID == p.Renewal.FromPriceID {
		return errors.New("provider amount contradicts the accepted recurring price")
	}
	initial, err := nmi.SubscriptionAmountMinor(nmi.V5Subscription{ID: frozen.SubscriptionID, Amount: frozen.Amount, Plan: &frozen.Plan}, p.Renewal.Currency)
	if err != nil || actual != initial {
		return errors.New("provider amount changed independently of this preparation")
	}
	wireAmount, err := nmi.WireAmount(p.AmountMinor, p.Renewal.Currency)
	if err != nil {
		return err
	}
	count, _ := strconv.Atoi(strings.TrimSpace(frozen.Plan.PlanPayments))
	if _, err := client.UpdateRecurringSubscription(ctx, p.RailSubscriptionID, wireAmount, count); err != nil {
		return fmt.Errorf("prepare recurring amount: %w", err)
	}
	after, found, err := client.GetSubscription(ctx, p.RailSubscriptionID)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("provider recurring obligation disappeared after preparation")
	}
	prepared, err := snapshotRebillPreparation(in, p, after)
	if err != nil {
		return err
	}
	prepared.Amount, prepared.Plan.PlanAmount = frozen.Amount, frozen.Plan.PlanAmount
	if !reflect.DeepEqual(prepared, frozen) {
		return errors.New("provider schedule changed during amount preparation")
	}
	amount, err := nmi.SubscriptionAmountMinor(after, p.Renewal.Currency)
	if err != nil || amount != p.AmountMinor {
		return errors.New("provider did not converge to the accepted recurring amount")
	}
	return nil
}
