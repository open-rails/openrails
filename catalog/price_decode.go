package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// UnmarshalJSON accepts readable money and duration aliases and normalizes
// them before validation and hashing. The serialized form carries native units.
func (p *ApplyPrice) UnmarshalJSON(raw []byte) error {
	type price ApplyPrice
	var decoded struct {
		price
		Amount          Field[string] `json:"amount,omitzero"`
		AccessDuration  Field[string] `json:"access_duration,omitzero"`
		BillingInterval Field[string] `json:"billing_interval,omitzero"`
		TrialDuration   Field[string] `json:"trial_duration,omitzero"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if decoded.Amount.Set {
		if decoded.Amount.Null {
			return fmt.Errorf("amount cannot be null")
		}
		if decoded.UnitAmount.Set || decoded.Currency.Set {
			return fmt.Errorf("amount cannot be combined with unit_amount or currency")
		}
		amount, currency, err := parseAmount(decoded.Amount.Value)
		if err != nil {
			return fmt.Errorf("amount: %w", err)
		}
		decoded.UnitAmount = Value(amount)
		decoded.Currency = Value(currency)
	}
	if decoded.BillingInterval.Set {
		if decoded.BillingIntervalHours.Set || decoded.BillingIntervalMonths.Set {
			return fmt.Errorf("billing_interval cannot be combined with billing_interval_hours or billing_interval_months")
		}
		decoded.BillingIntervalHours = Null[int]()
		if !decoded.BillingInterval.Null {
			hours, months, err := ParseBillingInterval(decoded.BillingInterval.Value)
			if err != nil {
				return err
			}
			if months > 0 {
				decoded.BillingIntervalMonths = Value(months)
			} else {
				decoded.BillingIntervalHours = Value(hours)
			}
		}
	}
	for _, field := range []struct {
		name  string
		text  Field[string]
		hours *Field[int]
	}{
		{"trial_duration", decoded.TrialDuration, &decoded.TrialDurationHours},
	} {
		if !field.text.Set {
			continue
		}
		if field.hours.Set {
			return fmt.Errorf("%s and %s_hours cannot both be set", field.name, field.name)
		}
		if field.text.Null {
			*field.hours = Null[int]()
			continue
		}
		hours, err := ParseDurationHours(field.text.Value)
		if err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
		*field.hours = Value(hours)
	}
	if decoded.AccessDuration.Set {
		if decoded.AccessDurationHours.Set {
			return fmt.Errorf("access_duration and access_duration_hours cannot both be set")
		}
		hours, err := parseAccessDuration(decoded.AccessDuration)
		if err != nil {
			return err
		}
		decoded.AccessDurationHours = hours
		if decoded.BillingIntervalMonths.Set && !decoded.BillingIntervalMonths.Null && hours.Null {
			return fmt.Errorf("a calendar price cannot grant access forever; omit access_duration to follow its billing period")
		}
	}
	*p = ApplyPrice(decoded.price)
	return nil
}
