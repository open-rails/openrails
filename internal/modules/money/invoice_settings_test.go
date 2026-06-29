package money

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPreviousInvoicePeriodBoundaries(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

	from, to, err := PreviousInvoicePeriod(now, time.Time{}, InvoiceBoundaryCalendarMonth)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), from)
	require.Equal(t, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), to)

	from, to, err = PreviousInvoicePeriod(now, time.Time{}, InvoiceBoundaryFixedInterval)
	require.NoError(t, err)
	require.Equal(t, fixedInvoiceInterval, to.Sub(from))
	require.Equal(t, int64(0), to.Unix()%int64(fixedInvoiceInterval/time.Second))

	anchor := time.Date(2026, 1, 31, 9, 30, 0, 0, time.UTC)
	from, to, err = PreviousInvoicePeriod(time.Date(2026, 3, 30, 10, 0, 0, 0, time.UTC), anchor, InvoiceBoundaryAnniversary)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 1, 31, 9, 30, 0, 0, time.UTC), from)
	require.Equal(t, time.Date(2026, 2, 28, 9, 30, 0, 0, time.UTC), to)
}

func TestNormalizeInvoiceBoundary(t *testing.T) {
	require.Equal(t, InvoiceBoundaryFixedInterval, NormalizeInvoiceBoundary(""))
	require.Equal(t, InvoiceBoundaryCalendarMonth, NormalizeInvoiceBoundary(" calendar_month "))
	require.Empty(t, NormalizeInvoiceBoundary("weekly"))
}
