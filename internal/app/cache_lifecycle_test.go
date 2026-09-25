package app

import (
	"testing"

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
