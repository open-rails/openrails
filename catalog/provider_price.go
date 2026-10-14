package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/currency"
)

// ApplyProviderPrice declares a price a provider owns and runs, linked to its
// product for entitlements. OpenRails reads its terms and never writes it:
// NMI plans and Stripe prices are read at apply, so only CCBill, which has no
// read API, declares its amount and billing interval. AccessDurationHours and
// OneOff default to the verified service-period end and recurring. Explicit
// null access is indefinite, while explicit hours override the native boundary. A declaration states
// the provider price whole; omitted Archived keeps its current value.
type ApplyProviderPrice struct {
	// PSP is the merchant's PSP key that owns the price.
	PSP string `json:"psp"`
	// PlanID is an NMI recurring plan.
	PlanID string `json:"plan_id,omitempty"`
	// PriceID is a Stripe price.
	PriceID string `json:"price_id,omitempty"`
	// FlexID and RecurringBillingOptionID are a CCBill FlexForm and its
	// recurring billing option.
	FlexID                   string `json:"flex_id,omitempty"`
	RecurringBillingOptionID string `json:"recurring_billing_option_id,omitempty"`
	// Currency prices an NMI plan, whose amount names none; USD when omitted.
	Currency             string     `json:"currency,omitempty"`
	UnitAmount           *int64     `json:"unit_amount,omitempty,string"`
	BillingIntervalHours *int       `json:"billing_interval_hours,omitempty"`
	AccessDurationHours  Field[int] `json:"access_duration_hours,omitzero"`
	TrialUnitAmount      *int64     `json:"trial_unit_amount,omitempty,string"`
	TrialDurationHours   *int       `json:"trial_duration_hours,omitempty"`
	// OneOff is a provider price that does not renew: its billing interval is
	// the access it sells.
	OneOff   bool        `json:"one_off,omitempty"`
	Archived Field[bool] `json:"archived,omitzero"`
}

// References are the provider identifiers the declaration names.
func (p ApplyProviderPrice) References() map[string]string {
	out := map[string]string{}
	for key, value := range map[string]string{"plan_id": p.PlanID, "price_id": p.PriceID, "flex_id": p.FlexID, "recurring_billing_option_id": p.RecurringBillingOptionID} {
		if value != "" {
			out[key] = value
		}
	}
	return out
}

// DeclaresTerms reports a declaration that states its own money terms.
func (p ApplyProviderPrice) DeclaresTerms() bool {
	return p.UnitAmount != nil || p.BillingIntervalHours != nil || p.TrialUnitAmount != nil || p.TrialDurationHours != nil
}

// UnmarshalJSON accepts the readable money and duration aliases prices take.
func (p *ApplyProviderPrice) UnmarshalJSON(raw []byte) error {
	type provider ApplyProviderPrice
	var decoded struct {
		provider
		UnitAmount      Field[int64]  `json:"unit_amount,omitzero"`
		TrialUnitAmount Field[int64]  `json:"trial_unit_amount,omitzero"`
		Amount          *string       `json:"amount,omitempty"`
		AccessDuration  Field[string] `json:"access_duration,omitempty"`
		BillingInterval *string       `json:"billing_interval,omitempty"`
		TrialDuration   *string       `json:"trial_duration,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	// Money reads as a number or a decimal string.
	for _, money := range []struct {
		field Field[int64]
		into  **int64
	}{{decoded.UnitAmount, &decoded.provider.UnitAmount}, {decoded.TrialUnitAmount, &decoded.provider.TrialUnitAmount}} {
		if money.field.Null {
			return fmt.Errorf("money cannot be null")
		}
		if money.field.Set {
			value := money.field.Value
			*money.into = &value
		}
	}
	if decoded.Amount != nil {
		if decoded.provider.UnitAmount != nil || decoded.Currency != "" {
			return fmt.Errorf("amount cannot be combined with unit_amount or currency")
		}
		amount, code, err := parseAmount(*decoded.Amount)
		if err != nil {
			return fmt.Errorf("amount: %w", err)
		}
		decoded.provider.UnitAmount, decoded.Currency = &amount, code
	}
	for _, field := range []struct {
		name  string
		text  *string
		hours **int
	}{
		{"billing_interval", decoded.BillingInterval, &decoded.BillingIntervalHours},
		{"trial_duration", decoded.TrialDuration, &decoded.TrialDurationHours},
	} {
		if field.text == nil {
			continue
		}
		if *field.hours != nil {
			return fmt.Errorf("%s and %s_hours cannot both be set", field.name, field.name)
		}
		hours, err := ParseDurationHours(*field.text)
		if err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
		*field.hours = &hours
	}
	if decoded.AccessDuration.Set {
		if decoded.AccessDurationHours.Set {
			return fmt.Errorf("access_duration and access_duration_hours cannot both be set")
		}
		decoded.AccessDurationHours = Null[int]()
		if !decoded.AccessDuration.Null {
			hours, err := ParseDurationHours(decoded.AccessDuration.Value)
			if err != nil {
				return fmt.Errorf("access_duration: %w", err)
			}
			decoded.AccessDurationHours = Value(hours)
		}
	}
	*p = ApplyProviderPrice(decoded.provider)
	return nil
}

// Validate checks the declaration's syntax; which references and terms its
// PSP's rail takes is checked against the merchant's PSPs at apply.
func (p ApplyProviderPrice) Validate() error {
	if strings.TrimSpace(p.PSP) == "" || strings.TrimSpace(p.PSP) != p.PSP {
		return fmt.Errorf("psp must name the PSP that owns the price")
	}
	refs := p.References()
	for key, value := range refs {
		if strings.TrimSpace(value) != value || len(value) > 255 {
			return fmt.Errorf("%s must be the provider's identifier", key)
		}
	}
	switch {
	case p.PlanID != "" && len(refs) == 1, p.PriceID != "" && len(refs) == 1:
	case p.PlanID == "" && p.PriceID == "" && len(refs) > 0:
	default:
		return fmt.Errorf("name the provider's object: plan_id (NMI), price_id (Stripe), or flex_id and recurring_billing_option_id (CCBill)")
	}
	if p.Currency != "" {
		if _, err := currency.ForPrice(p.Currency); err != nil {
			return err
		}
	}
	if (p.UnitAmount != nil) != (p.BillingIntervalHours != nil) || p.UnitAmount != nil && p.Currency == "" {
		return fmt.Errorf("declared terms take amount and billing_interval together")
	}
	if p.UnitAmount != nil && *p.UnitAmount < 0 || p.TrialUnitAmount != nil && *p.TrialUnitAmount < 0 {
		return fmt.Errorf("money cannot be negative")
	}
	if (p.TrialUnitAmount != nil) != (p.TrialDurationHours != nil) || p.TrialUnitAmount != nil && (p.OneOff || p.UnitAmount == nil) {
		return fmt.Errorf("a trial takes trial_unit_amount and trial_duration on recurring declared terms")
	}
	for _, hours := range []*int{p.BillingIntervalHours, p.TrialDurationHours} {
		if hours != nil && (*hours <= 0 || *hours > MaxDurationHours) {
			return fmt.Errorf("duration must be between 1 and %d hours", MaxDurationHours)
		}
	}
	if p.AccessDurationHours.Set && !p.AccessDurationHours.Null && (p.AccessDurationHours.Value <= 0 || p.AccessDurationHours.Value > MaxDurationHours) {
		return fmt.Errorf("access duration must be positive supported hours, or null")
	}
	return nil
}
