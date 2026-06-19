package middleware

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

// These white-box tests cover the in-memory fallback store's eviction internals.
// They live in package middleware (alongside RateLimitStore) so they can reach the
// unexported counters map and inMemoryCounter type directly.

func TestRateLimitStorePrunesExpiredCounters(t *testing.T) {
	t.Parallel()

	store := NewRateLimitStore()
	store.counters["expired"] = &inMemoryCounter{count: 1, reset: time.Now().Add(-time.Minute)}

	result := store.Allow("ip:203.0.113.30", "checkout", &config.RateLimit{RequestsPerMinute: 10})
	require.True(t, result.allowed)
	require.NotContains(t, store.counters, "expired")
}

func TestRateLimitStoreBoundsCounters(t *testing.T) {
	t.Parallel()

	store := NewRateLimitStore()
	for i := 0; i < maxInMemoryRateLimitCounters; i++ {
		store.counters[fmt.Sprintf("checkout:ip:203.0.113.%d", i)] = &inMemoryCounter{count: 1, reset: time.Now().Add(time.Hour)}
	}

	result := store.Allow("ip:203.0.113.31", "checkout", &config.RateLimit{RequestsPerMinute: 10})
	require.True(t, result.allowed)
	require.LessOrEqual(t, len(store.counters), maxInMemoryRateLimitCounters)
}

func TestRateLimitStoreDoesNotEvictWhenReusingExistingCounter(t *testing.T) {
	t.Parallel()

	store := NewRateLimitStore()
	ip := "203.0.113.32"
	key := "checkout:ip:" + ip
	store.counters[key] = &inMemoryCounter{count: 1, reset: time.Now().Add(time.Hour)}
	for i := 1; len(store.counters) < maxInMemoryRateLimitCounters; i++ {
		store.counters[fmt.Sprintf("checkout:ip:198.51.100.%d", i)] = &inMemoryCounter{count: 1, reset: time.Now().Add(time.Hour)}
	}

	result := store.Allow("ip:"+ip, "checkout", &config.RateLimit{RequestsPerMinute: 10})
	require.True(t, result.allowed)
	require.Len(t, store.counters, maxInMemoryRateLimitCounters)
	require.Contains(t, store.counters, key)
	require.Equal(t, 2, store.counters[key].count)
}
