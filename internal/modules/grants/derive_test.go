package grants

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
)

// Derived access is independent of both billing and cancellation dates.
func TestSubscriptionWindow(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	billingEnd, ended := start.Add(720*time.Hour), start.Add(100*time.Hour)
	for _, hours := range []*int32{new(int32(24)), new(int32(1000)), nil} {
		row := gen.ListUngrantedSubscriptionsRow{StartedAt: start, CurrentPeriodStartsAt: &start, CurrentPeriodEndsAt: &billingEnd, EndedAt: &ended, AccessDurationHoursSnapshot: hours}
		gotStart, gotEnd, ok := subscriptionWindow(row)
		require.True(t, ok)
		require.Equal(t, start, gotStart)
		if hours == nil {
			require.Nil(t, gotEnd)
		} else {
			require.Equal(t, start.Add(time.Duration(*hours)*time.Hour), *gotEnd)
		}
	}
	_, _, ok := subscriptionWindow(gen.ListUngrantedSubscriptionsRow{})
	require.False(t, ok)
	_, _, ok = subscriptionWindow(gen.ListUngrantedSubscriptionsRow{StartedAt: start, AccessDurationHoursSnapshot: new(int32(0))})
	require.False(t, ok)
}

func TestProductSpecKeys(t *testing.T) {
	for raw, want := range map[string][]string{
		`{"vip": null, "premium": 720}`: {"premium", "vip"},
		`{}`:                            {},
		`{"  premium  ": 1, "": 2}`:     {"premium"},
		`not-json`:                      nil,
		``:                              nil,
	} {
		require.Equal(t, want, productSpecKeys([]byte(raw)), raw)
	}
}

// An empty or inverted wallet window is a no-op that never reaches the database (nil queries).
func TestDeriveWalletGrantSkipsEmptyWindow(t *testing.T) {
	l := &Ledger{merchant: uuid.New()}
	now := time.Now().UTC()
	for _, exp := range []time.Time{now, now.Add(-time.Hour)} {
		require.NoError(t, l.DeriveWalletGrant(context.Background(), gen.ListUngrantedWalletPaymentsRow{ID: uuid.New(), CustomerID: uuid.New(), PurchasedAt: now, ExpiresAt: exp}))
	}
}
