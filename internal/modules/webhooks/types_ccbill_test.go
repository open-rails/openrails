package webhooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCCBillUpgradeSuccessEventUnmarshalFixture(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "..", "testdata", "webhooks", "ccbill", "upgradesuccess.json")
	payload, err := os.ReadFile(fixturePath)
	require.NoError(t, err)

	var event CCBillUpgradeSuccessEvent
	require.NoError(t, json.Unmarshal(payload, &event))

	require.NotEmpty(t, event.SubscriptionID)
	require.NotEmpty(t, event.TransactionID)
	require.NotEmpty(t, event.BilledInitialPrice)
	require.NotEmpty(t, event.OriginalSubscriptionID)
	require.NotEmpty(t, event.OriginalClientAccnum.Trimmed())
	require.NotEmpty(t, event.OriginalClientSubacc)
	require.NotEmpty(t, event.Source)
	require.NotEmpty(t, event.SCAResponseStatus)
}
