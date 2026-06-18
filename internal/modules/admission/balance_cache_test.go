package admission

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBalanceCache_HitMissTTL(t *testing.T) {
	c := NewBalanceCache(3 * time.Second)
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	c.SetClock(func() time.Time { return now })

	var calls int32
	load := func(av, cr int64) func() (int64, int64, error) {
		return func() (int64, int64, error) { atomic.AddInt32(&calls, 1); return av, cr, nil }
	}

	// miss → load.
	av, cr, err := c.Capacity("m", "cust", "USD", load(1000, 7))
	require.NoError(t, err)
	require.Equal(t, int64(1000), av)
	require.Equal(t, int64(7), cr)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))

	// hit within TTL → served from cache (loader NOT called, even though it would
	// return a different value).
	av, _, err = c.Capacity("m", "cust", "USD", load(9999, 0))
	require.NoError(t, err)
	require.Equal(t, int64(1000), av)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls), "served from cache")

	// advance past TTL → miss → reload.
	now = now.Add(4 * time.Second)
	av, _, err = c.Capacity("m", "cust", "USD", load(2000, 0))
	require.NoError(t, err)
	require.Equal(t, int64(2000), av)
	require.Equal(t, int32(2), atomic.LoadInt32(&calls))

	// A different payer is a separate entry (miss).
	_, _, err = c.Capacity("m", "other", "USD", load(500, 0))
	require.NoError(t, err)
	require.Equal(t, int32(3), atomic.LoadInt32(&calls))

	// A different currency for the same payer is also separate.
	_, _, err = c.Capacity("m", "cust", "EUR", load(600, 0))
	require.NoError(t, err)
	require.Equal(t, int32(4), atomic.LoadInt32(&calls))
}

func TestBalanceCache_NilReadsThrough(t *testing.T) {
	var c *BalanceCache // nil receiver
	var calls int32
	loader := func() (int64, int64, error) { atomic.AddInt32(&calls, 1); return 7, 1, nil }

	av, cr, err := c.Capacity("m", "cust", "USD", loader)
	require.NoError(t, err)
	require.Equal(t, int64(7), av)
	require.Equal(t, int64(1), cr)

	// A nil cache never caches: the second call loads again.
	_, _, err = c.Capacity("m", "cust", "USD", loader)
	require.NoError(t, err)
	require.Equal(t, int32(2), atomic.LoadInt32(&calls))
}

func TestBalanceCache_LoadErrorNotCached(t *testing.T) {
	c := NewBalanceCache(time.Minute)
	_, _, err := c.Capacity("m", "cust", "USD", func() (int64, int64, error) { return 0, 0, errors.New("boom") })
	require.Error(t, err)

	// The error was not cached: a subsequent call retries the loader.
	av, _, err := c.Capacity("m", "cust", "USD", func() (int64, int64, error) { return 42, 0, nil })
	require.NoError(t, err)
	require.Equal(t, int64(42), av)
}
