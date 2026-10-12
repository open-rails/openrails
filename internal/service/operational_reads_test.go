package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/retention"
)

func TestInspectionPageRetainsItsWindow(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	from := now.Add(-31 * retention.Day).Add(time.Microsecond)
	page, err := readInspectionPage("usage-events", billing.PageRequest{Limit: 2}, time.Time{}, time.Time{}, from, now, 31*retention.Day)
	require.NoError(t, err)
	_, err = readInspectionPage("usage-events", billing.PageRequest{}, now.Add(-31*retention.Day), time.Time{}, from, now, 31*retention.Day)
	require.NoError(t, err, "the default inclusive-current-instant upper bound permits a full 31-day window")
	position := page.position("usage-events", now.Add(-time.Hour), "event-id")
	cursor := pagination.Encode(position)
	next, err := readInspectionPage("usage-events", billing.PageRequest{Cursor: cursor}, time.Time{}, time.Time{}, now, now.Add(24*time.Hour), 31*retention.Day)
	require.NoError(t, err)
	require.Equal(t, page.from, next.from)
	require.Equal(t, page.to, next.to, "a subsequent page does not silently slide the occurrence window")
	_, err = readInspectionPage("admissions", billing.PageRequest{Cursor: cursor}, time.Time{}, time.Time{}, from, now, 0)
	require.ErrorIs(t, err, pagination.ErrInvalidCursor)
	_, err = readInspectionPage("usage-events", billing.PageRequest{Cursor: cursor}, from.Add(time.Hour), time.Time{}, from, now, 31*retention.Day)
	require.ErrorIs(t, err, pagination.ErrInvalidCursor)
	_, err = readInspectionPage("usage-events", billing.PageRequest{Limit: billing.MaxPageLimit + 1}, from, now, from, now, 31*retention.Day)
	require.ErrorIs(t, err, pagination.ErrInvalidLimit)
	_, err = readInspectionPage("usage-events", billing.PageRequest{}, now.Add(-32*retention.Day), now, from, now, 31*retention.Day)
	require.Error(t, err)
}
