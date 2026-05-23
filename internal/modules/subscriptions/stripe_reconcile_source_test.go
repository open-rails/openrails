package subscriptions

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseStripeSubscriptionList(t *testing.T) {
	body := []byte(`{
		"object": "list",
		"has_more": true,
		"data": [
			{
				"id": "sub_active",
				"status": "active",
				"customer": "cus_1",
				"current_period_start": 1770000100,
				"current_period_end": 1770000200,
				"cancel_at_period_end": false,
				"metadata": {"user_id": "user-1", "internal_price_id": "p-1"},
				"items": {"data": [{"price": {"id": "price_a"}}]}
			},
			{
				"id": "sub_canceled",
				"status": "canceled",
				"customer": "cus_2",
				"metadata": {},
				"items": {"data": []}
			}
		]
	}`)

	subs, hasMore, err := parseStripeSubscriptionList(body)
	require.NoError(t, err)
	require.True(t, hasMore)
	require.Len(t, subs, 2)

	require.Equal(t, "sub_active", subs[0].ID)
	require.Equal(t, "active", subs[0].Status)
	require.Equal(t, "cus_1", subs[0].CustomerID)
	require.Equal(t, "price_a", subs[0].StripePriceID)
	require.Equal(t, "user-1", subs[0].Metadata["user_id"])
	require.Equal(t, int64(1770000100), subs[0].CurrentPeriodStart.Unix())
	require.Equal(t, int64(1770000200), subs[0].CurrentPeriodEnd.Unix())
	require.False(t, subs[0].CancelAtPeriodEnd)

	require.Equal(t, "sub_canceled", subs[1].ID)
	require.Empty(t, subs[1].StripePriceID)
}

func TestParseStripeSubscriptionListRejectsGarbage(t *testing.T) {
	_, _, err := parseStripeSubscriptionList([]byte("not json"))
	require.Error(t, err)
}

func TestStripeSubscriptionStatusActive(t *testing.T) {
	for _, status := range []string{"active", "trialing", "ACTIVE"} {
		require.True(t, StripeSubscriptionStatusActive(status), status)
	}
	for _, status := range []string{"past_due", "canceled", "incomplete", "incomplete_expired", "unpaid", ""} {
		require.False(t, StripeSubscriptionStatusActive(status), status)
	}
}
