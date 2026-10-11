package service

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// A usage report request: its defaults, its page size, and a cursor that
// reads only the rest of the report it came from.
func TestReadUsageParams(t *testing.T) {
	now := time.Date(2040, 5, 15, 12, 30, 0, 0, time.UTC)
	read := func(p billing.GetUsageParams) (usageReport, error) {
		if p.Currency == "" {
			p.Currency = "usd"
		}
		return readUsageParams(p, func() time.Time { return now })
	}
	refusedQuery := func(t *testing.T, err error, param string) {
		t.Helper()
		var refusal *apperr.Error
		require.True(t, errors.As(err, &refusal), "%v", err)
		require.Equal(t, billing.CodeInvalidQuery, refusal.Code)
		require.Equal(t, param, refusal.Param)
	}

	got, err := read(billing.GetUsageParams{})
	require.NoError(t, err)
	require.Equal(t, usageReport{currency: "USD", groupBy: billing.UsageByEventType, from: now.AddDate(0, -1, 0), to: now, limit: billing.DefaultUsagePageLimit}, got)

	for _, limit := range []int{1, billing.MaxUsagePageLimit} {
		got, err := read(billing.GetUsageParams{PageRequest: billing.PageRequest{Limit: limit}})
		require.NoError(t, err)
		require.Equal(t, limit, got.limit)
	}
	for _, limit := range []int{-1, billing.MaxUsagePageLimit + 1} {
		_, err := read(billing.GetUsageParams{PageRequest: billing.PageRequest{Limit: limit}})
		require.ErrorIs(t, err, pagination.ErrInvalidLimit, limit)
	}
	_, err = read(billing.GetUsageParams{GroupBy: "tier"})
	refusedQuery(t, err, "group_by")
	_, err = read(billing.GetUsageParams{From: now, To: now})
	refusedQuery(t, err, "from")
	_, err = read(billing.GetUsageParams{To: now.AddDate(0, -2, 0)})
	refusedQuery(t, err, "from")

	from, to := time.Date(2040, 4, 1, 0, 0, 0, 0, time.UTC), time.Date(2040, 4, 30, 23, 59, 59, 123456789, time.UTC)
	issued := usageCursor{Amount: 5_000, Key: "gpu", Currency: "USD", GroupBy: billing.UsageByResource, From: from, To: to}
	cursor := pagination.Encode(issued)
	page := func(p billing.GetUsageParams) (usageReport, error) {
		p.GroupBy, p.Cursor = billing.UsageByResource, cursor
		return read(p)
	}

	t.Run("a cursor reads the rest of its report", func(t *testing.T) {
		got, err := page(billing.GetUsageParams{})
		require.NoError(t, err)
		require.True(t, got.from.Equal(from) && got.to.Equal(to), "a window left out is the cursor's, not the month before now")
		require.Equal(t, &usageCursor{Amount: 5_000, Key: "gpu", Currency: "USD", GroupBy: billing.UsageByResource, From: from, To: to}, got.after)

		named, err := page(billing.GetUsageParams{From: from.In(time.FixedZone("x", 3600)), To: to})
		require.NoError(t, err, "the same window, named")
		require.True(t, named.from.Equal(from))
	})

	t.Run("a cursor of another report is refused", func(t *testing.T) {
		for name, p := range map[string]billing.GetUsageParams{
			"currency": {Currency: "EUR"},
			"from":     {From: from.Add(time.Second)},
			"to":       {To: to.Add(-time.Nanosecond)},
		} {
			_, err := page(p)
			require.ErrorIs(t, err, pagination.ErrInvalidCursor, name)
		}
		_, err := read(billing.GetUsageParams{GroupBy: billing.UsageByInvoker, PageRequest: billing.PageRequest{Cursor: cursor}})
		require.ErrorIs(t, err, pagination.ErrInvalidCursor, "group_by")
	})

	t.Run("a foreign or altered cursor is refused", func(t *testing.T) {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		require.NoError(t, err)
		for name, c := range map[string]string{
			"another list's":   pagination.Encode(pagination.TimeID{At: now, ID: uuid.New()}),
			"another currency": base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), `"c":"USD"`, `"c":"EUR"`, 1))),
			"without a window": pagination.Encode(usageCursor{Amount: 1, Key: "gpu", Currency: "USD", GroupBy: billing.UsageByResource}),
			"truncated":        cursor[:len(cursor)-3],
			"not a cursor":     "page-2",
		} {
			p := billing.GetUsageParams{GroupBy: billing.UsageByResource, PageRequest: billing.PageRequest{Cursor: c}}
			_, err := read(p)
			require.ErrorIs(t, err, pagination.ErrInvalidCursor, name)
		}
	})
}
