//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// A reprice batch moves a price key's subscribers to its current version: it
// is created, read, listed and canceled as one resource, and its reprices are
// read and canceled one by one.
func TestRepriceBatches(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	client := w.client[embedded]
	old := w.membership("content:reprice", 10_000_000)
	member := w.newCustomer()
	sub := member.subscribe(embedded, "stripe", old.ID.String(), "content:reprice", member.saveCard("stripe", visa))

	preview, err := client.PreviewRepriceBatch(ctx, billing.RepriceBatchPreviewParams{PriceKey: old.Key})
	require.NoError(t, err)
	require.Equal(t, 1, preview.Matched, "the preview counts the whole chain before the new version exists")

	hours := monthHours
	next, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: old.ProductID, Key: old.Key, UnitAmount: 12_000_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours})
	require.NoError(t, err)
	effective := w.clock.Now().Add(45 * 24 * time.Hour).UTC().Truncate(time.Second)
	create := billing.CreateRepriceBatchParams{PriceKey: old.Key, EffectiveAt: effective}

	first, err := client.CreateRepriceBatch(ctx, create)
	require.NoError(t, err)
	require.Equal(t, next.ID, first.ToPriceID)
	require.Equal(t, 1, first.Matched)
	require.Len(t, first.Scheduled, 1)
	require.Empty(t, first.Skipped)
	require.Equal(t, sub, first.Scheduled[0].SubscriptionID)
	require.NotNil(t, first.Scheduled[0].RepriceID)

	batch, err := client.GetRepriceBatch(ctx, first.BatchID)
	require.NoError(t, err)
	require.Equal(t, billing.RepriceKindReprice, batch.Kind)
	require.Equal(t, old.Key, *batch.PriceKey)
	require.Equal(t, next.ID, batch.ToPriceID)
	require.Equal(t, [5]int{1, 0, 1, 0, 0}, [5]int{batch.Matched, batch.Skipped, batch.Scheduled, batch.Applied, batch.Canceled})

	reprices, err := client.ListReprices(ctx, billing.RepriceListParams{RepriceBatchID: first.BatchID})
	require.NoError(t, err)
	require.Len(t, reprices.Items, 1)
	reprice := reprices.Items[0]
	require.Equal(t, *first.Scheduled[0].RepriceID, reprice.ID)
	require.Equal(t, billing.RepriceScheduled, reprice.Status)
	require.Equal(t, old.ID, reprice.FromPriceID)
	require.Equal(t, first.BatchID, *reprice.RepriceBatchID)
	got, err := client.GetReprice(ctx, reprice.ID)
	require.NoError(t, err)
	require.Equal(t, reprice, *got)
	bySubscription, err := client.ListReprices(ctx, billing.RepriceListParams{SubscriptionID: sub, Status: billing.RepriceScheduled})
	require.NoError(t, err)
	require.Equal(t, []billing.Reprice{reprice}, bySubscription.Items)

	// One scheduled reprice per subscription: a second batch skips it.
	second, err := client.CreateRepriceBatch(ctx, create)
	require.NoError(t, err)
	require.Empty(t, second.Scheduled)
	require.Len(t, second.Skipped, 1)
	require.Nil(t, second.Skipped[0].RepriceID)
	require.NotNil(t, second.Skipped[0].Reason)

	canceled, err := client.CancelReprice(ctx, reprice.ID)
	require.NoError(t, err)
	require.Equal(t, billing.RepriceCanceled, canceled.Status)
	require.NotNil(t, canceled.CanceledAt)
	_, err = client.CancelReprice(ctx, reprice.ID)
	requireCode(t, err, http.StatusConflict, "reprice_not_scheduled")
	batch, err = client.GetRepriceBatch(ctx, first.BatchID)
	require.NoError(t, err)
	require.Equal(t, [2]int{0, 1}, [2]int{batch.Scheduled, batch.Canceled})

	// A batch cancel cancels what is still scheduled.
	third, err := client.CreateRepriceBatch(ctx, create)
	require.NoError(t, err)
	require.Len(t, third.Scheduled, 1)
	cancel, err := client.CancelRepriceBatch(ctx, third.BatchID)
	require.NoError(t, err)
	require.Equal(t, 1, cancel.Canceled)
	require.Empty(t, cancel.RailReleaseRequired)
	require.Nil(t, cancel.Warning)
	cancel, err = client.CancelRepriceBatch(ctx, third.BatchID)
	require.NoError(t, err)
	require.Zero(t, cancel.Canceled, "nothing is left to cancel")

	// Batches list newest first, one cursor page at a time.
	var listed []billing.RepriceBatchID
	page := billing.RepriceBatchListParams{PageRequest: billing.PageRequest{Limit: 2}, PriceKey: old.Key}
	for {
		batches, err := client.ListRepriceBatches(ctx, page)
		require.NoError(t, err)
		for _, b := range batches.Items {
			listed = append(listed, b.ID)
		}
		if batches.Next == "" {
			break
		}
		page.Cursor = batches.Next
	}
	require.Equal(t, []billing.RepriceBatchID{third.BatchID, second.BatchID, first.BatchID}, listed)

	_, err = client.GetRepriceBatch(ctx, billing.RepriceBatchID(member.customerID()))
	requireCode(t, err, http.StatusNotFound, "resource_not_found")
	_, err = client.GetReprice(ctx, billing.RepriceID(member.customerID()))
	requireCode(t, err, http.StatusNotFound, "reprice_not_found")
}
