package reconcile

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStripeDisputeOutcomeControlsAccessRecommendation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, status string }{
		{"open", "needs_response"},
		{"under review", "under_review"},
		{"lost", "lost"},
		{"won", "won"},
		{"missing outcome", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := LocalSubscription{ID: uuid.New(), CustomerID: uuid.New(), Status: "active", Rail: "stripe"}
			payment := LocalPayment{ID: uuid.New(), TransactionID: "ch_disputed", SubscriptionID: &sub.ID}
			raw := json.RawMessage(fmt.Sprintf(`{"id":"dp_test","charge":"ch_disputed","status":%q,"amount":999,"currency":"usd","created":1791763200}`, tc.status))
			dispute := normalizeStripeDispute(raw)
			snapshot := &RemoteSnapshot{Provider: ProviderStripe, Capabilities: Capabilities{Transactions: true, Chargebacks: true}, Transactions: []RemoteTransaction{dispute}}
			findings := diffProvider(ProviderStripe, snapshot, &LocalState{Subscriptions: []LocalSubscription{sub}}, []LocalPayment{payment}, time.Unix(1791849600, 0), diffOptions{})
			if tc.status == "won" {
				assert.Empty(t, findings, "winning the dispute removes the reason to revoke access")
			} else {
				require.Len(t, findings, 1)
				assert.Equal(t, FindingChargebackActiveSub, findings[0].Type)
				assert.True(t, findings[0].RequiresAdmin)
				require.NotNil(t, findings[0].Change)
				require.NotNil(t, findings[0].Change.Apply.RevokeAccess)
				assert.Equal(t, sub.ID, findings[0].Change.Apply.RevokeAccess.SubscriptionID)
			}
			assert.Equal(t, []RemoteTransaction{dispute}, snapshot.Transactions, "the dispute remains in the snapshot")
			assert.Equal(t, TransactionTypeChargeback, dispute.Type)
			assert.True(t, dispute.Success, "the chargeback happened regardless of its outcome")
			assert.JSONEq(t, string(raw), string(dispute.Raw))
		})
	}
}
