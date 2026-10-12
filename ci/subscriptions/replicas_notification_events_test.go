//go:build e2e && integration

package subscriptions_test

import (
	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
)

// Two hosts sharing the billing database consume the same immutable event
// independently; a replica's acknowledgment is not another app's delivery.
func TestReplicasConsumeNotificationEventsIndependently(t *testing.T) {
	t.Parallel()
	f := startFleet(t, 3, false, nil)
	a, b := f.named("a"), f.named("b")
	first, err := a.client[embedded].NotificationEvents("consumer-a")
	require.NoError(t, err)
	require.NoError(t, first.Register(t.Context()))
	second, err := b.client[embedded].NotificationEvents("consumer-b")
	require.NoError(t, err)
	require.NoError(t, second.Register(t.Context()))
	life := a.lifetime("replicas:receipt", 25_000_000)
	c := a.newCustomer()
	card := c.saveCard("nmi", visa)
	paid := c.order(http.MethodPost, "/orders", "buy-"+uuid.NewString(), map[string]any{"lines": []any{line(life, 0)}, "expected_total": micros(25_000_000), "payment": map[string]any{"payment_method_id": card}})
	require.Equal(t, "complete", paid.body["status"], "%v", paid.body)
	events, err := first.Pending(t.Context(), 100)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.True(t, events[0].Optional)
	require.NoError(t, first.Acknowledge(t.Context(), []billing.NotificationEventRef{{MerchantID: events[0].MerchantID, ID: events[0].ID}}))
	again, err := first.Pending(t.Context(), 100)
	require.NoError(t, err)
	require.Empty(t, again)
	independent, err := second.Pending(t.Context(), 100)
	require.NoError(t, err)
	require.Equal(t, events, independent)
	require.Len(t, c.receipts(), 1)
}
