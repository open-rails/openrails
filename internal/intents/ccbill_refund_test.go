package intents

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCCBillRefundAlwaysRequiresOperatorVerification(t *testing.T) {
	h := NewCCBillRefundHandler(nil, nil)
	for _, attempts := range []int32{1, 2, 4} {
		p := testRefundPayload()
		p.ProviderTarget = "sub_123"
		p.ProviderTransactionID = "requested_charge"
		row := refundIntent(t, TypeCCBillRefund, p)
		row.Attempts = attempts
		reason := "ccbill refund denied (-7): old unqualified refusal"
		row.LastFailureReason = &reason
		for _, result := range []Outcome{h.Execute(context.Background(), row), h.Verify(context.Background(), row)} {
			require.Equal(t, OutcomeAmbiguous, result.Class)
			require.Contains(t, result.Reason, "operator must verify")
		}
	}
	payload, evidence := h.PrunePolicy()
	require.True(t, payload)
	require.True(t, evidence)
}

func TestCCBillExplicitAuthenticationDenialBudget(t *testing.T) {
	require.False(t, ccbillDenialExhausted(1))
	require.True(t, ccbillDenialExhausted(ccbillDenialMaxAttempts))
}
