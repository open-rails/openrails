//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// Consumers receive the collection policy recorded with the billed order,
// without looking up a customer's mutable invoice profile or assuming a card.
func TestOrderDueEventsRecordCollectionMethod(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.settleRefreshes()
	w.jobs.PeriodicJobs().Clear()
	ctx, client := t.Context(), w.client[remote]
	consumer, err := w.client[embedded].NotificationEvents("order-due-method")
	require.NoError(t, err)
	require.NoError(t, consumer.Register(ctx))
	declareDynamicMeter(t, client, "order-due-method", "units")
	const owed = int64(50_000_000)
	for _, method := range []billing.InvoiceCollectionMethod{billing.CollectChargeAutomatically, billing.CollectSendInvoice} {
		t.Run(string(method), func(t *testing.T) {
			customer := w.newCustomer()
			params := billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 2 * owed}}}
			if method == billing.CollectSendInvoice {
				params.InvoiceProfile = catalog.Value(billing.InvoiceProfile{CollectionMethod: method, NetTermsDays: 30})
			}
			_, err := client.UpdateCustomer(ctx, customer.cid(), params)
			require.NoError(t, err)
			_, err = recordUsage(ctx, client, billing.RecordUsageParams{CustomerID: customer.cid(), Invoker: customer.id,
				Currency: "USD", EventType: "order-due-method", Amount: new(owed), Dimensions: map[string]int64{"units": 1}, Source: "test", SourceID: uuid.NewString()})
			require.NoError(t, err)
			w.advance(time.Minute)
			w.runPass(invoicePass{})
			orders, err := client.ListOrders(ctx, billing.OrderListParams{Origin: billing.OrderOriginUsage, CustomerID: customer.cid()})
			require.NoError(t, err)
			require.Len(t, orders.Items, 1)
			order := orders.Items[0]
			eventsForOrder := func() []billing.NotificationEvent {
				events, err := consumer.Pending(ctx, 100)
				require.NoError(t, err)
				var found []billing.NotificationEvent
				for _, event := range events {
					if event.CustomerID == customer.cid() && event.EventType == "order_due" {
						found = append(found, event)
					}
				}
				return found
			}
			events := eventsForOrder()
			require.Len(t, events, 1)
			var data billing.NotificationData
			require.NoError(t, json.Unmarshal(events[0].Data, &data))
			require.Equal(t, method, data.CollectionMethod)
			require.Equal(t, order.ID, data.OrderID)
			require.NotNil(t, data.AmountDue)
			require.Equal(t, owed, *data.AmountDue)
			require.Equal(t, "USD", data.Currency)
			require.NotNil(t, data.DueAt)
			require.True(t, order.Usage.DueAt.Equal(*data.DueAt))
			w.runPass(invoicePass{})
			require.Equal(t, events, eventsForOrder(), "a repeated billing pass preserves the immutable fact")
		})
	}
}
