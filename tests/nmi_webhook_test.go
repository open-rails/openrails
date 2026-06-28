package tests

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/modules/webhooks/replay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNMIWebhookPayloads tests loading and validating NMI webhook payloads
func TestNMIWebhookPayloads(t *testing.T) {
	t.Run("Load Subscription Add Payload", func(t *testing.T) {
		payload, err := replay.LoadTestWebhookPayload("mobius", "recurring_subscription_add.json")
		require.NoError(t, err, "Should load subscription add payload")
		assert.NotEmpty(t, payload, "Payload should not be empty")

		// Validate JSON structure
		var events []map[string]interface{}
		err = json.Unmarshal([]byte(payload), &events)
		require.NoError(t, err, "Should be valid JSON array")
		assert.Greater(t, len(events), 0, "Should have at least one event")

		// Check first event structure
		event := events[0]
		assert.Contains(t, event, "event_id", "Should have event_id")
		assert.Contains(t, event, "event_type", "Should have event_type")
		assert.Contains(t, event, "event_body", "Should have event_body")

		eventType, ok := event["event_type"].(string)
		require.True(t, ok, "event_type should be string")
		assert.Equal(t, "recurring.subscription.add", eventType, "Should be subscription add event")

		t.Logf("Loaded NMI subscription add payload with %d events", len(events))
	})

	t.Run("Load Subscription Update Payload", func(t *testing.T) {
		payload, err := replay.LoadTestWebhookPayload("mobius", "recurring_subscription_update.json")
		require.NoError(t, err, "Should load subscription update payload")
		assert.NotEmpty(t, payload, "Payload should not be empty")

		var events []map[string]interface{}
		err = json.Unmarshal([]byte(payload), &events)
		require.NoError(t, err, "Should be valid JSON array")

		if len(events) > 0 {
			event := events[0]
			eventType, ok := event["event_type"].(string)
			require.True(t, ok, "event_type should be string")
			assert.Equal(t, "recurring.subscription.update", eventType, "Should be subscription update event")
		}

		t.Logf("Loaded NMI subscription update payload")
	})

	t.Run("Load Subscription Delete Payload", func(t *testing.T) {
		payload, err := replay.LoadTestWebhookPayload("mobius", "recurring_subscription_delete.json")
		require.NoError(t, err, "Should load subscription delete payload")
		assert.NotEmpty(t, payload, "Payload should not be empty")

		var events []map[string]interface{}
		err = json.Unmarshal([]byte(payload), &events)
		require.NoError(t, err, "Should be valid JSON array")

		if len(events) > 0 {
			event := events[0]
			eventType, ok := event["event_type"].(string)
			require.True(t, ok, "event_type should be string")
			assert.Equal(t, "recurring.subscription.delete", eventType, "Should be subscription delete event")
		}

		t.Logf("Loaded NMI subscription delete payload")
	})

	t.Run("Invalid Payload File", func(t *testing.T) {
		_, err := replay.LoadTestWebhookPayload("mobius", "nonexistent.json")
		assert.Error(t, err, "Should fail for nonexistent file")
		assert.Contains(t, err.Error(), "failed to read payload file", "Should have appropriate error message")
	})
}

// TestNMIWebhookValidation tests webhook payload validation
func TestNMIWebhookValidation(t *testing.T) {
	t.Run("Validate Subscription Add Event", func(t *testing.T) {
		err := replay.ValidateEvent("mobius", "recurring_subscription_add.json")
		assert.NoError(t, err, "Should validate subscription add event")
		t.Log("NMI subscription add event validated successfully")
	})

	t.Run("Validate Subscription Update Event", func(t *testing.T) {
		err := replay.ValidateEvent("mobius", "recurring_subscription_update.json")
		assert.NoError(t, err, "Should validate subscription update event")
		t.Log("NMI subscription update event validated successfully")
	})

	t.Run("Validate Subscription Delete Event", func(t *testing.T) {
		err := replay.ValidateEvent("mobius", "recurring_subscription_delete.json")
		assert.NoError(t, err, "Should validate subscription delete event")
		t.Log("NMI subscription delete event validated successfully")
	})

	t.Run("Validate All NMI Events", func(t *testing.T) {
		err := replay.ValidateAllEvents("mobius")
		assert.NoError(t, err, "Should validate all NMI events")
		t.Log("All NMI events validated successfully")
	})

	t.Run("Invalid Rail", func(t *testing.T) {
		err := replay.ValidateEvent("invalid", "test.json")
		assert.Error(t, err, "Should fail with invalid rail")
		assert.Contains(t, err.Error(), "invalid rail", "Should have appropriate error message")
	})
}

func TestStringishSubscriptionIDNormalization(t *testing.T) {
	payload, err := replay.LoadTestWebhookPayload("mobius", "recurring_subscription_add.json")
	require.NoError(t, err, "Should load payload")

	var events []webhooks.NMIWebhookEvent
	require.NoError(t, json.Unmarshal([]byte(payload), &events))
	require.NotEmpty(t, events, "expected at least one event payload")

	for _, evt := range events {
		var body webhooks.NMIRecurringEventBody
		require.NoError(t, json.Unmarshal(evt.EventBody, &body))

		subID := body.SubscriptionID.Trimmed()
		assert.NotEmpty(t, subID, "subscription_id should not be empty")
		assert.False(t, strings.Contains(strings.ToLower(subID), "e+"), "subscription_id must not use scientific notation")
	}
}
