package webhooks

import (
	"encoding/json"
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
)

// #675: the stripe event-created watermark stored in subscription metadata is
// the ordering guard for out-of-order webhook delivery.
func TestStripeSubEventCreatedRoundTrip(t *testing.T) {
	sub := &models.Subscription{}
	if got := stripeSubEventCreated(sub.Metadata); got != 0 {
		t.Fatalf("empty metadata watermark = %d, want 0", got)
	}

	setStripeSubEventCreated(sub, 1750000000)
	if got := stripeSubEventCreated(sub.Metadata); got != 1750000000 {
		t.Fatalf("watermark = %d, want 1750000000", got)
	}

	// Overwrite advances.
	setStripeSubEventCreated(sub, 1750000005)
	if got := stripeSubEventCreated(sub.Metadata); got != 1750000005 {
		t.Fatalf("watermark = %d, want 1750000005", got)
	}

	// Non-positive created is ignored (events without created must not clear it).
	setStripeSubEventCreated(sub, 0)
	if got := stripeSubEventCreated(sub.Metadata); got != 1750000005 {
		t.Fatalf("watermark after zero set = %d, want 1750000005", got)
	}
}

func TestStripeSubEventCreatedPreservesOtherMetadata(t *testing.T) {
	sub := &models.Subscription{Metadata: json.RawMessage(`{"checkout_session_id":"cs_1"}`)}
	setStripeSubEventCreated(sub, 42)

	var m map[string]any
	if err := json.Unmarshal(sub.Metadata, &m); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if m["checkout_session_id"] != "cs_1" {
		t.Fatalf("existing metadata key clobbered: %v", m)
	}
	if got := stripeSubEventCreated(sub.Metadata); got != 42 {
		t.Fatalf("watermark = %d, want 42", got)
	}
}

func TestStripeSubEventCreatedToleratesGarbage(t *testing.T) {
	if got := stripeSubEventCreated(json.RawMessage(`not-json`)); got != 0 {
		t.Fatalf("garbage metadata watermark = %d, want 0", got)
	}
	if got := stripeSubEventCreated(json.RawMessage(`{"stripe_last_event_created":"nope"}`)); got != 0 {
		t.Fatalf("non-numeric watermark = %d, want 0", got)
	}
}
