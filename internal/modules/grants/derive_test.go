package grants

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
)

// Explicit derived access is independent of both billing and cancellation
// dates; a provider default ends with the paid period and is never indefinite.
func TestSubscriptionWindow(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	billingEnd, ended := start.Add(720*time.Hour), start.Add(100*time.Hour)
	for _, hours := range []*int32{new(int32(24)), new(int32(1000)), nil} {
		row := gen.ListUngrantedSubscriptionsRow{StartedAt: start, CurrentPeriodStartsAt: &start, CurrentPeriodEndsAt: &billingEnd, EndedAt: &ended, AccessDurationHoursSnapshot: hours}
		gotStart, gotEnd, ok := subscriptionWindow(row.StartedAt, row.CurrentPeriodStartsAt, row.CurrentPeriodEndsAt, row.AccessDurationHoursSnapshot, row.ProviderAccessDefaultSnapshot)
		require.True(t, ok)
		require.Equal(t, start, gotStart)
		if hours == nil {
			require.Nil(t, gotEnd)
		} else {
			require.Equal(t, start.Add(time.Duration(*hours)*time.Hour), *gotEnd)
		}
	}
	gotStart, gotEnd, ok := subscriptionWindow(start, &start, &billingEnd, nil, true)
	require.True(t, ok)
	require.Equal(t, start, gotStart)
	require.Equal(t, billingEnd, *gotEnd)
	for _, end := range []*time.Time{nil, &start} {
		_, _, ok = subscriptionWindow(start, &start, end, nil, true)
		require.False(t, ok, "no verified service end grants nothing")
	}
	_, _, ok = subscriptionWindow(time.Time{}, nil, nil, nil, false)
	require.False(t, ok)
	_, _, ok = subscriptionWindow(start, nil, nil, new(int32(0)), false)
	require.False(t, ok)
}

// An empty or inverted wallet window is a no-op that never reaches the database (nil queries).
func TestDeriveWalletGrantSkipsEmptyWindow(t *testing.T) {
	l := &Ledger{merchant: uuid.New()}
	now := time.Now().UTC()
	for _, exp := range []time.Time{now, now.Add(-time.Hour)} {
		require.NoError(t, l.DeriveWalletGrant(context.Background(), gen.ListUngrantedWalletPaymentsRow{ID: uuid.New(), CustomerID: uuid.New(), PurchasedAt: now, ExpiresAt: exp}))
	}
}
