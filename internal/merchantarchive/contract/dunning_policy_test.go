package contract

import (
	"encoding/json"
	"testing"

	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/stretchr/testify/require"
)

func TestDunningPolicyArchive(t *testing.T) {
	defaults, err := json.Marshal(collection.DeclaredOf(collection.DefaultPolicy))
	require.NoError(t, err)
	for _, tc := range []struct {
		name, policy string
		valid        bool
	}{
		{"default snapshot", string(defaults), true},
		{"custom snapshot", `{"tiers":[{"max_cycle_hours":96},{"retry_after_hours":[24,48]}],"transient_retry_minutes":[5,10],"access_during_dunning":"suspend","access_while_renewal_held":"keep"}`, true},
		{"runtime defaults", `{}`, true},
		{"unknown field", `{"card_token":"secret"}`, false},
		{"unknown tier field", `{"tiers":[{"retry":24}]}`, false},
		{"fractional hours", `{"tiers":[{"max_cycle_hours":96.5},{}]}`, false},
		{"string hours", `{"tiers":[{"max_cycle_hours":"96"},{}]}`, false},
		{"null tier", `{"tiers":[null]}`, false},
		{"invalid access", `{"access_during_dunning":"forever"}`, false},
		{"invalid held access", `{"access_while_renewal_held":"forever"}`, false},
		{"nonincreasing retries", `{"tiers":[{"max_cycle_hours":96},{"retry_after_hours":[48,24]}]}`, false},
		{"retries outlast cycle", `{"tiers":[{"retry_after_hours":[24]}]}`, false},
		{"invalid transient", `{"transient_retry_minutes":[0]}`, false},
		{"overflow hours", `{"tiers":[{"max_cycle_hours":5124096},{}]}`, false},
		{"overflow minutes", `{"transient_retry_minutes":[307445735]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, values := row(t, "subscriptions", map[string]string{"rail": "nmi", "collection_policy": "engine", "dunning_policy": tc.policy})
			err := ValidateValues(p, values)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "subscriptions.dunning_policy")
			}
		})
	}
}

func TestRecoveryOrderPaymentErrorArchive(t *testing.T) {
	for _, tc := range []struct {
		name, failure string
		valid         bool
	}{
		{"decline", `{"reason":"insufficient_funds","message":"Your card has insufficient funds."}`, true},
		{"card correction", `{"reason":"incorrect_cvc","message":"Check your security code.","field":"cvc"}`, true},
		{"unknown field", `{"reason":"declined","raw_response":"unreviewed"}`, false},
		{"wrong type", `{"reason":202,"message":"Declined"}`, false},
		{"sensitive text", `{"reason":"declined","message":"4111111111111111"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, values := row(t, "orders", map[string]string{"last_payment_error": tc.failure})
			err := ValidateValues(p, values)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "orders.last_payment_error")
			}
		})
	}
}

func TestRecoveryCollectionOrderReferenceArchive(t *testing.T) {
	require.NoError(t, validateJSON("provider_intents.subscription_collection.payload", `{"order_id":"`+testMerchant+`"}`))
	require.Error(t, validateJSON("provider_intents.subscription_collection.payload", `{"order_id":"not-an-order-id"}`))
}
