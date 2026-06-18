package admission

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPolicyCache_TierHitMissTTL(t *testing.T) {
	c := NewPolicyCache(10 * time.Minute)
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	c.SetClock(func() time.Time { return now })

	var calls int32
	load := func(cur string) func() (PayerSpendLimits, error) {
		return func() (PayerSpendLimits, error) {
			atomic.AddInt32(&calls, 1)
			return PayerSpendLimits{PolicyCurrency: cur}, nil
		}
	}

	// miss → load.
	pol, err := c.PayerSpendLimits("m", "cust", "tier_1", load("USD"))
	require.NoError(t, err)
	require.Equal(t, "USD", pol.PolicyCurrency)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))

	// hit within TTL → cached (loader not called).
	pol, err = c.PayerSpendLimits("m", "cust", "tier_1", load("EUR"))
	require.NoError(t, err)
	require.Equal(t, "USD", pol.PolicyCurrency)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))

	// different tier → separate key (miss).
	_, err = c.PayerSpendLimits("m", "cust", "tier_2", load("USD"))
	require.NoError(t, err)
	require.Equal(t, int32(2), atomic.LoadInt32(&calls))

	// different merchant, same payer+tier → separate key (miss).
	_, err = c.PayerSpendLimits("m2", "cust", "tier_1", load("USD"))
	require.NoError(t, err)
	require.Equal(t, int32(3), atomic.LoadInt32(&calls))

	// advance past the 10-min TTL → reload.
	now = now.Add(11 * time.Minute)
	_, err = c.PayerSpendLimits("m", "cust", "tier_1", load("USD"))
	require.NoError(t, err)
	require.Equal(t, int32(4), atomic.LoadInt32(&calls))
}

func TestPolicyCache_BudgetPoliciesHitMiss(t *testing.T) {
	c := NewPolicyCache(time.Hour)
	var calls int32
	load := func() ([]InvokerSpendLimit, error) {
		atomic.AddInt32(&calls, 1)
		return []InvokerSpendLimit{{Scope: "invoker", ScopeKey: "user:a"}}, nil
	}

	got, err := c.InvokerSpendLimits("m", "cust", load)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))

	// hit.
	_, err = c.InvokerSpendLimits("m", "cust", load)
	require.NoError(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))

	// different payer → miss.
	_, err = c.InvokerSpendLimits("m", "other", load)
	require.NoError(t, err)
	require.Equal(t, int32(2), atomic.LoadInt32(&calls))
}

func TestPolicyCache_NilReadsThrough_AndErrorNotCached(t *testing.T) {
	var c *PolicyCache // nil
	var calls int32
	_, err := c.PayerSpendLimits("m", "cust", "t", func() (PayerSpendLimits, error) {
		atomic.AddInt32(&calls, 1)
		return PayerSpendLimits{}, nil
	})
	require.NoError(t, err)
	_, err = c.PayerSpendLimits("m", "cust", "t", func() (PayerSpendLimits, error) {
		atomic.AddInt32(&calls, 1)
		return PayerSpendLimits{}, nil
	})
	require.NoError(t, err)
	require.Equal(t, int32(2), atomic.LoadInt32(&calls), "nil cache never caches")

	live := NewPolicyCache(time.Hour)
	_, err = live.InvokerSpendLimits("m", "cust", func() ([]InvokerSpendLimit, error) { return nil, errors.New("boom") })
	require.Error(t, err)
	// error not cached → retry loads again.
	var n int32
	_, err = live.InvokerSpendLimits("m", "cust", func() ([]InvokerSpendLimit, error) { atomic.AddInt32(&n, 1); return nil, nil })
	require.NoError(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&n))
}
