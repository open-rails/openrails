package money

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

func TestBillingPeriods(t *testing.T) {
	d := func(y int, m time.Month, day, h, mi int) time.Time {
		return time.Date(y, m, day, h, mi, 0, 0, time.UTC)
	}
	anchor := d(2026, 1, 31, 9, 30)
	for _, tt := range []struct {
		name, boundary string
		now, anchor    time.Time
		from, to       time.Time
	}{
		{"calendar month", " calendar_month ", d(2026, 6, 29, 12, 0), time.Time{}, d(2026, 5, 1, 0, 0), d(2026, 6, 1, 0, 0)},
		{"calendar month crosses year", BillingBoundaryCalendarMonth, d(2026, 1, 15, 0, 0), time.Time{}, d(2025, 12, 1, 0, 0), d(2026, 1, 1, 0, 0)},
		{"anniversary clamps to short month", BillingBoundaryAnniversary, d(2026, 3, 30, 10, 0), anchor, anchor, d(2026, 2, 28, 9, 30)},
		{"anniversary after this month's date", BillingBoundaryAnniversary, d(2026, 3, 31, 10, 0), anchor, d(2026, 2, 28, 9, 30), d(2026, 3, 31, 9, 30)},
	} {
		from, to, err := PreviousBillingPeriod(tt.now, tt.anchor, tt.boundary)
		require.NoError(t, err, tt.name)
		require.Equal(t, tt.from, from, tt.name)
		require.Equal(t, tt.to, to, tt.name)
	}

	// Fixed interval (also the blank default): epoch-aligned, exactly one interval, ending at or before now.
	now := d(2026, 6, 29, 12, 0)
	from, to, err := PreviousBillingPeriod(now, time.Time{}, "")
	require.NoError(t, err)
	require.Equal(t, fixedBillingInterval, to.Sub(from))
	require.Zero(t, to.Unix()%int64(fixedBillingInterval/time.Second))
	require.False(t, to.After(now))
	require.True(t, to.Add(fixedBillingInterval).After(now))

	_, _, err = PreviousBillingPeriod(now, time.Time{}, "weekly")
	require.Error(t, err)
	for in, want := range map[string]string{
		"": BillingBoundaryFixedInterval, " Calendar_Month ": BillingBoundaryCalendarMonth,
		"anniversary": BillingBoundaryAnniversary, "weekly": "",
	} {
		require.Equal(t, want, merchantconfig.NormalizeBillingBoundary(in), in)
	}
}

// Validation runs before the (nil) query handle is touched.
func TestPendingOwedItemGuards(t *testing.T) {
	ctx, merchantID, customerID := context.Background(), uuid.New(), uuid.New()
	at := time.Now()
	require.NoError(t, insertPendingOwedItemTx(ctx, nil, merchantID, customerID, "USD", "usage", "r-1", 0, at, nil), "zero amount is a no-op")
	require.EqualError(t, insertPendingOwedItemTx(ctx, nil, merchantID, customerID, "USD", " ", "r-1", 100, at, nil),
		"owed item source_type and source_id required")
	require.EqualError(t, insertPendingOwedItemTx(ctx, nil, merchantID, customerID, "USD", "usage", "r-1", 100, time.Time{}, nil),
		"owed item timestamp required")
}
