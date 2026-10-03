package checkout

import (
	"encoding/json"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/decline"
)

// operationFailure renders a refused card operation's retained provider
// evidence as the customer-facing decline. Raw codes stay in the evidence.
func operationFailure(in gen.BillingRailIntent) *billing.PaymentFailure {
	failure := operationReason(in).Failure()
	return &failure
}

// operationReason classifies a refused card operation's retained provider
// evidence.
func operationReason(in gen.BillingRailIntent) billing.DeclineReason {
	evidence := map[string]json.RawMessage{}
	_ = json.Unmarshal(in.ResultEvidence, &evidence)
	if raw, ok := evidence["qualified_initial_refusal"]; ok {
		nested := map[string]json.RawMessage{}
		if json.Unmarshal(raw, &nested) == nil {
			for key, value := range nested {
				evidence[key] = value
			}
		}
	}
	get := func(keys ...string) string {
		for _, key := range keys {
			raw, ok := evidence[key]
			if !ok {
				continue
			}
			var text string
			if json.Unmarshal(raw, &text) == nil {
				if text = strings.TrimSpace(text); text != "" {
					return text
				}
				continue
			}
			var number json.Number
			if json.Unmarshal(raw, &number) == nil && number.String() != "0" {
				return number.String()
			}
		}
		return ""
	}
	if get("declined") != "" || get("localization_id", "decline_code", "stripe_decline_code", "failure_code", "stripe_failure_code", "response_code") != "" {
		return decline.ClassifyEvidence(decline.Evidence{
			Rail:         in.Rail,
			Code:         get("localization_id", "decline_code", "stripe_decline_code", "response_code", "failure_code", "stripe_failure_code"),
			FallbackCode: get("stripe_failure_code", "failure_code"),
			AVS:          get("avs_response"),
			CVV:          get("cvv_response"),
		}).Reason
	}
	return billing.DeclineProcessingError
}
