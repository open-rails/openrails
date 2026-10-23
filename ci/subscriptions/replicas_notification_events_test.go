//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/testevents"
	"github.com/stretchr/testify/require"
)

// Two hosts sharing the billing database consume the same immutable event
// independently; reading changes no shared source state.
func TestReplicasConsumeNotificationEventsIndependently(t *testing.T) {
	t.Parallel()
	f := startFleet(t, 3, false, nil)
	a, b := f.named("a"), f.named("b")
	first := testevents.Notifications(a.client[embedded])
	second := testevents.Notifications(b.client[embedded])
	life := a.lifetime("replicas:receipt", 25_000_000)
	c := a.newCustomer()
	card := c.saveCard("nmi", visa)
	paid := c.order(http.MethodPost, "/orders", "buy-"+uuid.NewString(), map[string]any{"lines": []any{line(life, 0)}, "expected_total": micros(25_000_000), "payment": map[string]any{"payment_method_id": card}})
	require.Equal(t, "complete", paid.body["status"], "%v", paid.body)
	events, err := first.Read(t.Context())
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.True(t, events[0].Optional)
	again, err := first.Read(t.Context())
	require.NoError(t, err)
	require.Equal(t, events, again, "reading never consumes the retained facts")
	independent, err := second.Read(t.Context())
	require.NoError(t, err)
	require.Equal(t, events, independent)
	require.Len(t, c.receipts(), 1)
}
