package app

import (
	"bytes"
	"context"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/replaycache"
	"github.com/open-rails/openrails/pkg/cache"
	"github.com/stretchr/testify/require"
)

type ownedTestCache struct {
	cache.Cache
	closed int
}

func (c *ownedTestCache) Close() error { c.closed++; return nil }

func TestClosePreservesHostCacheAndClosesFallback(t *testing.T) {
	// Redis may replace the selected cache backend; the memory fallback is
	// still owned, while supplied backends remain the host's responsibility.
	host, fallback := &ownedTestCache{}, &ownedTestCache{}
	a := &App{Cache: cache.NewSwitchableCache(host), ownedCache: fallback}
	require.NoError(t, a.Close(t.Context()))
	require.NoError(t, a.Close(t.Context()))
	require.Zero(t, host.closed)
	require.Equal(t, 1, fallback.closed)
}

func TestRuntimeCloseStopsBothReplayCleanups(t *testing.T) {
	// Observe actual worker exit rather than only checking a Close call. This
	// test is sequential so unrelated app tests do not create replay workers.
	workers := func() int {
		var stacks bytes.Buffer
		require.NoError(t, pprof.Lookup("goroutine").WriteTo(&stacks, 2))
		return bytes.Count(stacks.Bytes(), []byte("replaycache.(*Store).cleanupLoop"))
	}
	before := workers()
	rt := &Runtime{IdempotencyService: replaycache.NewStore(nil), webhookIdempotencyService: replaycache.NewStore(nil)}
	t.Cleanup(func() { rt.IdempotencyService.Close(); rt.webhookIdempotencyService.Close() })
	require.Eventually(t, func() bool { return workers() == before+2 }, time.Second, time.Millisecond)
	require.NoError(t, rt.Close(context.Background()))
	require.NoError(t, rt.Close(context.Background()))
	require.Eventually(t, func() bool { return workers() == before }, time.Second, time.Millisecond, "both runtime-owned replay workers must exit")
}
