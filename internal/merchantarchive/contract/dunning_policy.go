package contract

import (
	"encoding/json"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/collection"
)

// Keep the case's saved policy intact; deployment defaults cannot replace it.
func dunningPolicyJSON(v any) bool {
	if !object(map[string]jsonRule{
		"tiers":                   array(object(map[string]jsonRule{"max_cycle_hours": integerValue, "retry_after_hours": array(integerValue)})),
		"transient_retry_minutes": array(integerValue), "access_during_dunning": textValue, "access_while_renewal_held": textValue,
	})(v) {
		return false
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return false
	}
	var declared billing.DunningPolicy
	if json.Unmarshal(raw, &declared) != nil {
		return false
	}
	// PolicyOf converts integers to durations. Refuse overflow before that conversion.
	for _, tier := range declared.Tiers {
		if tier.MaxCycleHours < 0 || tier.MaxCycleHours > catalog.MaxDurationHours {
			return false
		}
		for _, hours := range tier.RetryAfterHours {
			// Leave room for collection validation's maximum 24-hour window allowance.
			if hours <= 0 || hours > catalog.MaxDurationHours-24 {
				return false
			}
		}
	}
	for _, minutes := range declared.TransientRetryMinutes {
		if minutes < 1 || minutes > 120 {
			return false
		}
	}
	_, err = collection.PolicyOf(&declared)
	return err == nil
}
