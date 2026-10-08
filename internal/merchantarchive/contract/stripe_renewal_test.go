package contract

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStripeRenewalReceiptCoverageSurvivesArchive(t *testing.T) {
	// This canonical engine digest contains a PAN-shaped digit run. The typed
	// field accepts it without treating arbitrary free text as a safe hash.
	const digest = "cf0b56594a544b069795b7521f553ad1ab9dbc4413daf5369343142307519d66"
	for _, tc := range []struct {
		digest string
		valid  bool
	}{{digest, true}, {strings.ToUpper(digest), false}, {"4111111111111111", false}} {
		raw := fmt.Sprintf(`{"qualified_receipt":{"stripe_engine":{"renewal_terms_sha256":%q}}}`, tc.digest)
		err := validateJSON("provider_intents.subscription_collection.result_evidence", raw)
		require.Equal(t, tc.valid, err == nil, "%v", err)
	}
	require.NoError(t, validateJSON("provider_intents.subscription_collection.result_evidence", `{"qualified_receipt":{"stripe_engine":{}}}`), "the new optional field does not rewrite older receipts")
}
