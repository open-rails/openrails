package contract

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeclinedSaleEvidenceArchive(t *testing.T) {
	require.NoError(t, validateJSON("provider_intents.nmi_sale.result_evidence", `{"declined":true,"response_code":202,"avs_response":"N","cvv_response":"M","decline_transaction_id":"9000000003"}`))
	for _, raw := range []string{
		`{"avs_response":123}`,
		`{"cvv_response":false}`,
		`{"decline_transaction_id":9000000003}`,
		`{"avs_response":"4111111111111111"}`,
		`{"cvv_response":"4111111111111111"}`,
		`{"decline_transaction_id":"4111111111111111"}`,
		`{"raw_response":"unreviewed"}`,
	} {
		require.Error(t, validateJSON("provider_intents.nmi_sale.result_evidence", raw), raw)
	}
}
