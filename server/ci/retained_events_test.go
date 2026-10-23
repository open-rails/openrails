//go:build e2e && integration

package ci_test

import (
	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/server/internal/operator"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRetainedEventsPrivilegedDeletedMerchant(t *testing.T) {
	w := newWorld(t)
	graph, _ := operator.Of(w.srv)
	ctx := t.Context()
	eventID := uuid.New()
	require.NoError(t, graph.Runtime.NotificationService.CreateIfAbsent(merchant.WithID(ctx, w.shop), &models.NotificationEvent{
		ID: eventID, CustomerID: uuid.New(), EventType: models.NotificationOrderDue, Data: billing.NotificationData{Message: "retained"},
	}))
	client := handle(t, w.srv, w.shop)
	live, err := client.ListEvents(ctx, billing.EventQuery{Types: []string{string(models.NotificationOrderDue)}})
	require.NoError(t, err)
	require.Len(t, live.Events, 1)
	_, err = w.srv.DeleteMerchant(ctx, w.shop)
	require.NoError(t, err)
	_, err = client.ListEvents(ctx, billing.EventQuery{Types: []string{string(models.NotificationOrderDue)}})
	require.Error(t, err, "ordinary client must not regain deleted merchant authority")
	retained, err := w.srv.ListEvents(ctx, w.shop, billing.EventQuery{Types: []string{string(models.NotificationOrderDue)}})
	require.NoError(t, err)
	require.Len(t, retained.Events, 1)
	require.Equal(t, "ntf_"+eventID.String(), retained.Events[0].ID)
	_, err = w.srv.ListEvents(ctx, w.rival, billing.EventQuery{After: retained.NextCursor, Types: []string{string(models.NotificationOrderDue)}})
	require.Error(t, err, "privileged reads still cannot transfer a merchant cursor")
}
