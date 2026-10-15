package contract

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecurringConfirmationArchiveRejectsUnknownAndSensitiveFields(t *testing.T) {
	for _, tc := range []struct{ field, raw string }{
		{"payload", `{"security_key":"secret"}`},
		{"payload", `{"instrument":{"rail_customer_ref":"4111111111111111"}}`},
		{"payload", `{"instrument":{"payment_token":"ephemeral"}}`},
		{"result_evidence", `{"confirmation_answer":{"raw_response":"secret"}}`},
		{"result_evidence", `{"confirmation_answer":{"transaction_id":"sk_live_secret","response_code":100}}`},
		{"result_evidence", `{"confirmation_answer":{"transaction_id":"verify-1","response_code":"100"}}`},
	} {
		require.Error(t, validateJSON("provider_intents.nmi_recurring_confirmation."+tc.field, tc.raw), tc.raw)
	}
}
