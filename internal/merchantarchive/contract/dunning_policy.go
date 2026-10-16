package contract

import (
	"encoding/json"

	"github.com/open-rails/openrails/billing"
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
	_, err = collection.PolicyOf(&declared)
	return err == nil
}
