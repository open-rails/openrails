package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStringishSubscriptionIDNormalization guards against NMI subscription ids
// arriving in scientific notation: the Stringish decoder must normalize the
// fixture's numeric subscription_id to a plain digit string.
func TestStringishSubscriptionIDNormalization(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("..", "testdata", "webhooks", "nmi", "recurring_subscription_add.json"))
	require.NoError(t, err, "Should load payload")

	var events []webhooks.NMIWebhookEvent
	require.NoError(t, json.Unmarshal(payload, &events))
	require.NotEmpty(t, events, "expected at least one event payload")

	for _, evt := range events {
		var body webhooks.NMIRecurringEventBody
		require.NoError(t, json.Unmarshal(evt.EventBody, &body))

		subID := body.SubscriptionID.Trimmed()
		assert.NotEmpty(t, subID, "subscription_id should not be empty")
		assert.False(t, strings.Contains(strings.ToLower(subID), "e+"), "subscription_id must not use scientific notation")
	}
}
