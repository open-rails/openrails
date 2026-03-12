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

	t.Run("Invalid Processor", func(t *testing.T) {
		err := replay.ValidateEvent("invalid", "test.json")
		assert.Error(t, err, "Should fail with invalid processor")
		assert.Contains(t, err.Error(), "invalid processor", "Should have appropriate error message")
	})
}

// TestNMIWebhookStructure tests the structure of NMI webhook payloads
func TestNMIWebhookStructure(t *testing.T) {
	t.Run("Subscription Add Structure", func(t *testing.T) {
		payload, err := replay.LoadTestWebhookPayload("mobius", "recurring_subscription_add.json")
		require.NoError(t, err, "Should load payload")

		var events []map[string]interface{}
		err = json.Unmarshal([]byte(payload), &events)
		require.NoError(t, err, "Should parse JSON")

		if len(events) > 0 {
			event := events[0]

			// Check required top-level fields
			assert.Contains(t, event, "event_id", "Should have event_id")
			assert.Contains(t, event, "event_type", "Should have event_type")
			assert.Contains(t, event, "event_body", "Should have event_body")

			// Check event_body structure
			eventBody, ok := event["event_body"].(map[string]interface{})
			require.True(t, ok, "event_body should be an object")

			// Check common event_body fields
			expectedFields := []string{"subscription_id", "billing_address", "plan"}
			for _, field := range expectedFields {
				assert.Contains(t, eventBody, field, "Should have %s field", field)
			}

			// Check billing_address structure
			if billingAddr, ok := eventBody["billing_address"].(map[string]interface{}); ok {
				assert.Contains(t, billingAddr, "email", "Should have email in billing_address")
				assert.Contains(t, billingAddr, "first_name", "Should have first_name in billing_address")
				assert.Contains(t, billingAddr, "last_name", "Should have last_name in billing_address")
			}

			// Check plan structure
			if plan, ok := eventBody["plan"].(map[string]interface{}); ok {
				assert.Contains(t, plan, "id", "Should have id in plan")
				assert.Contains(t, plan, "amount", "Should have amount in plan")
			}

			t.Log("NMI subscription add structure validated")
		}
	})

	t.Run("Event ID Format", func(t *testing.T) {
		payload, err := replay.LoadTestWebhookPayload("mobius", "recurring_subscription_add.json")
		require.NoError(t, err, "Should load payload")

		var events []map[string]interface{}
		err = json.Unmarshal([]byte(payload), &events)
		require.NoError(t, err, "Should parse JSON")

		if len(events) > 0 {
			event := events[0]
			eventID, ok := event["event_id"].(string)
			require.True(t, ok, "event_id should be string")
			assert.NotEmpty(t, eventID, "event_id should not be empty")

			// Check if it looks like a UUID (basic check)
			assert.Contains(t, eventID, "-", "event_id should contain hyphens (UUID format)")
			assert.True(t, len(eventID) > 30, "event_id should be reasonably long")

			t.Logf("Event ID format validated: %s", eventID)
		}
	})
}

// TestNMIWebhookCustomization tests customizing webhook payloads for testing
func TestNMIWebhookCustomization(t *testing.T) {
	t.Run("Customize Subscription ID", func(t *testing.T) {
		payload, err := replay.LoadTestWebhookPayload("mobius", "recurring_subscription_add.json")
		require.NoError(t, err, "Should load payload")

		var events []map[string]interface{}
		err = json.Unmarshal([]byte(payload), &events)
		require.NoError(t, err, "Should parse JSON")

		if len(events) > 0 {
			// Customize the subscription ID
			event := events[0]
			eventBody := event["event_body"].(map[string]interface{})
			originalSubID := eventBody["subscription_id"]

			customSubID := "test-subscription-12345"
			eventBody["subscription_id"] = customSubID

			// Verify customization
			assert.Equal(t, customSubID, eventBody["subscription_id"], "Should have custom subscription ID")
			assert.NotEqual(t, originalSubID, eventBody["subscription_id"], "Should be different from original")

			// Convert back to JSON to ensure it's still valid
			customizedPayload, err := json.Marshal(events)
			require.NoError(t, err, "Should marshal customized payload")
			assert.Contains(t, string(customizedPayload), customSubID, "Should contain custom subscription ID")

			t.Logf("Successfully customized subscription ID to: %s", customSubID)
		}
	})

	t.Run("Customize Email Address", func(t *testing.T) {
		payload, err := replay.LoadTestWebhookPayload("mobius", "recurring_subscription_add.json")
		require.NoError(t, err, "Should load payload")

		var events []map[string]interface{}
		err = json.Unmarshal([]byte(payload), &events)
		require.NoError(t, err, "Should parse JSON")

		if len(events) > 0 {
			event := events[0]
			eventBody := event["event_body"].(map[string]interface{})
			billingAddr := eventBody["billing_address"].(map[string]interface{})

			customEmail := "test-user@example.com"
			billingAddr["email"] = customEmail

			assert.Equal(t, customEmail, billingAddr["email"], "Should have custom email")

			t.Logf("Successfully customized email to: %s", customEmail)
		}
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
