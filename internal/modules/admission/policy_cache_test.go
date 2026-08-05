package admission

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPolicyCache_ResolutionHitMissTTL(t *testing.T) {
	c := NewPolicyCache(10 * time.Minute)
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	c.SetClock(func() time.Time { return now })

	var calls int32
	load := func(name string) func() (ResolvedPolicy, error) {
		return func() (ResolvedPolicy, error) {
			atomic.AddInt32(&calls, 1)
			return ResolvedPolicy{Name: name}, nil
		}
	}

	// miss → load.
	pol, err := c.ResolvedPolicy("m", "cust", "trust_1", load("standard"))
	require.NoError(t, err)
	require.Equal(t, "standard", pol.Name)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))

	// hit within TTL → cached (loader not called).
	pol, err = c.ResolvedPolicy("m", "cust", "trust_1", load("other"))
	require.NoError(t, err)
	require.Equal(t, "standard", pol.Name)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))

	// different tier → separate key (miss).
	_, err = c.ResolvedPolicy("m", "cust", "trust_2", load("standard"))
	require.NoError(t, err)
	require.Equal(t, int32(2), atomic.LoadInt32(&calls))

	// different merchant, same payer+tier → separate key (miss).
	_, err = c.ResolvedPolicy("m2", "cust", "trust_1", load("standard"))
	require.NoError(t, err)
	require.Equal(t, int32(3), atomic.LoadInt32(&calls))

	// advance past the 10-min TTL → reload.
	now = now.Add(11 * time.Minute)
	_, err = c.ResolvedPolicy("m", "cust", "trust_1", load("standard"))
	require.NoError(t, err)
	require.Equal(t, int32(4), atomic.LoadInt32(&calls))
}

// A rebinding must take effect on the NEXT admit, not at the end of the TTL: a
// merchant that tightens or switches a policy has revoked the old one, and
// serving it for another 15 minutes keeps admitting under a cap nobody chose.
func TestPolicyCache_InvalidateOnBindingChange(t *testing.T) {
	c := NewPolicyCache(time.Hour)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	c.SetClock(func() time.Time { return now })

	load := func(name string) func() (ResolvedPolicy, error) {
		return func() (ResolvedPolicy, error) { return ResolvedPolicy{Name: name}, nil }
	}

	pol, err := c.ResolvedPolicy("m", "cust", "free", load("generous"))
	require.NoError(t, err)
	require.Equal(t, "generous", pol.Name)

	// Still cached before any write.
	pol, err = c.ResolvedPolicy("m", "cust", "free", load("strict"))
	require.NoError(t, err)
	require.Equal(t, "generous", pol.Name)

	// Rebinding retires every entry for THAT merchant only.
	_, err = c.ResolvedPolicy("other", "cust", "free", load("neighbour"))
	require.NoError(t, err)
	c.InvalidateMerchant("m")

	pol, err = c.ResolvedPolicy("m", "cust", "free", load("strict"))
	require.NoError(t, err)
	require.Equal(t, "strict", pol.Name, "rebinding must be visible on the next admit")

	pol, err = c.ResolvedPolicy("other", "cust", "free", load("changed"))
	require.NoError(t, err)
	require.Equal(t, "neighbour", pol.Name, "one merchant's rebinding must not flush another's")
}

func TestPolicyCache_NilReadsThrough_AndErrorNotCached(t *testing.T) {
	var c *PolicyCache // nil
	var calls int32
	_, err := c.ResolvedPolicy("m", "cust", "t", func() (ResolvedPolicy, error) {
		atomic.AddInt32(&calls, 1)
		return ResolvedPolicy{}, nil
	})
	require.NoError(t, err)
	_, err = c.ResolvedPolicy("m", "cust", "t", func() (ResolvedPolicy, error) {
		atomic.AddInt32(&calls, 1)
		return ResolvedPolicy{}, nil
	})
	require.NoError(t, err)
	require.Equal(t, int32(2), atomic.LoadInt32(&calls), "nil cache never caches")

	live := NewPolicyCache(time.Hour)
	_, err = live.ResolvedPolicy("m", "cust", "t", func() (ResolvedPolicy, error) { return ResolvedPolicy{}, errors.New("boom") })
	require.Error(t, err)
	// error not cached → retry loads again.
	var n int32
	_, err = live.ResolvedPolicy("m", "cust", "t", func() (ResolvedPolicy, error) { atomic.AddInt32(&n, 1); return ResolvedPolicy{}, nil })
	require.NoError(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&n))
}
