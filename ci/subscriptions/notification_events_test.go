//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"

	"github.com/open-rails/openrails/internal/testevents"
	"github.com/stretchr/testify/require"
)

// notificationEvents reads source facts through the host's independent consumer.
// Customer-facing inbox APIs are deliberately absent from OpenRails.
func (c *customer) notificationEvents() []any {
	t := c.w.t
	t.Helper()
	consumer := testevents.Notifications(c.w.client[embedded])
	events, err := consumer.Read(t.Context())
	require.NoError(t, err)
	out := []any{}
	for _, event := range events {
		if event.CustomerID != c.cid() {
			continue
		}
		raw, err := json.Marshal(event)
		require.NoError(t, err)
		var wire map[string]any
		require.NoError(t, json.Unmarshal(raw, &wire))
		out = append(out, wire)
	}
	return out
}
func (c *customer) notificationData(kind string) map[string]any {
	c.w.t.Helper()
	for _, item := range c.notificationEvents() {
		n := item.(map[string]any)
		if n["event_type"] == kind {
			return n["data"].(map[string]any)
		}
	}
	c.w.t.Fatalf("no notification event %s", kind)
	return nil
}
