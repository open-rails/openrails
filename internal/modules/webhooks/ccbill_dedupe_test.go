package webhooks

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/stretchr/testify/require"
)

func TestStableDedupeEventKey_UsesTransactionID(t *testing.T) {
	t.Parallel()

	body, err := json.Marshal(map[string]any{
		"transactionId":  "txn_123",
		"subscriptionId": "sub_123",
		"timestamp":      "2026-02-17 12:00:00",
	})
	require.NoError(t, err)

	svc := &CCBillWebhookService{Data: CCBillWebhookEvent{EventType: EventTypeRenewalSuccess, EventBody: body}}
	key := svc.stableDedupeEventKey()
	require.Equal(t, "tx:txn_123", key)
}

func TestStableDedupeEventKey_IsStableForNonTransactionEvent(t *testing.T) {
	t.Parallel()

	bodyA := []byte(`{"subscriptionId":"sub_123","timestamp":"2026-02-17 12:00:00","clientAccnum":"945280","clientSubacc":"0000","reason":"cancelled by user"}`)
	bodyB := []byte(`{"reason":"cancelled by user","clientSubacc":"0000","subscriptionId":"sub_123","clientAccnum":"945280","timestamp":"2026-02-17 12:00:00"}`)

	svcA := &CCBillWebhookService{Data: CCBillWebhookEvent{EventType: EventTypeCancellation, EventBody: bodyA}}
	svcB := &CCBillWebhookService{Data: CCBillWebhookEvent{EventType: EventTypeCancellation, EventBody: bodyB}}

	keyA := svcA.stableDedupeEventKey()
	keyB := svcB.stableDedupeEventKey()
	require.Equal(t, keyA, keyB)
	require.Contains(t, keyA, "ccbill:event:")
}

func TestHandleCCBillWebhook_RejectsAccountMismatch(t *testing.T) {
	body := []byte(`{"subscriptionId":"sub_123","transactionId":"txn_123","timestamp":"2026-02-17 12:00:00","clientAccnum":"wrong","clientSubacc":"0000"}`)
	svc := &CCBillWebhookService{
		Data: CCBillWebhookEvent{EventType: EventTypeRenewalSuccess, EventBody: body},
		CCBillClient: ccbill.NewRESTClient(&config.CCBillConfig{
			ClientAccNum: "945280",
			ClientSubAcc: "0000",
		}),
	}

	err := svc.HandleCCBillWebhook(context.Background())
	require.Error(t, err)
	require.True(t, IsWebhookErrorNonRetryable(err))
}
