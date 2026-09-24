package checkout

import (
	"encoding/json"
	"strings"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/payments"
)

// operationFailure renders a refused card operation's retained provider
// evidence as the customer-facing decline. Raw codes stay in the evidence.
func operationFailure(in gen.OpenrailsRailIntent) *openrails.PaymentFailure {
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
		failure := payments.CustomerDecline(payments.DeclineDetail{
			Rail:         in.Rail,
			Code:         get("localization_id", "decline_code", "stripe_decline_code", "response_code", "failure_code", "stripe_failure_code"),
			FallbackCode: get("stripe_failure_code", "failure_code"),
			AVS:          get("avs_response"),
			CVV:          get("cvv_response"),
		})
		return &failure
	}
	failure := payments.NewPaymentFailure(payments.DeclineProcessingError)
	return &failure
}
