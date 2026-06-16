//go:build integration

package holds_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/holds"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

func newHoldStore(t *testing.T) (*holds.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	c, err := tcredis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	conn, err := c.ConnectionString(ctx)
	require.NoError(t, err)
	opt, err := redis.ParseURL(conn)
	require.NoError(t, err)
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(ctx).Err())
	return holds.NewStore(rdb), ctx
}

func TestStorePlaceBlocksConcurrentOverspend(t *testing.T) {
	store, ctx := newHoldStore(t)
	merchantID := uuid.NewString()
	customerID := uuid.NewString()
	expires := time.Now().Add(time.Hour).UTC()

	var wg sync.WaitGroup
	results := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, _, err := store.Place(ctx, holds.Hold{
				MerchantID:      merchantID,
				RequestID:       fmt.Sprintf("req-%d", i),
				CustomerID:      customerID,
				Invoker:         fmt.Sprintf("user:%d", i),
				InvokerType:     "delegated",
				Currency:        "USD",
				EstimatedAmount: 30,
				Source:          "invoke",
				Resource:        "endpoint:stable",
				ExpiresAt:       expires,
			}, 100)
			require.NoError(t, err)
			results <- ok
		}(i)
	}
	wg.Wait()
	close(results)

	allowed := 0
	for ok := range results {
		if ok {
			allowed++
		}
	}
	require.Equal(t, 3, allowed)
	active, err := store.ActiveAmount(ctx, merchantID, customerID, "USD")
	require.NoError(t, err)
	require.Equal(t, int64(90), active)
}

func TestStoreReleaseFreesCapacity(t *testing.T) {
	store, ctx := newHoldStore(t)
	merchantID := uuid.NewString()
	customerID := uuid.NewString()
	expires := time.Now().Add(time.Hour).UTC()
	base := holds.Hold{
		MerchantID:      merchantID,
		CustomerID:      customerID,
		Invoker:         "user:1",
		InvokerType:     "delegated",
		Currency:        "USD",
		EstimatedAmount: 50,
		Source:          "invoke",
		Resource:        "endpoint:stable",
		ExpiresAt:       expires,
	}

	first := base
	first.RequestID = "req-1"
	ok, active, err := store.Place(ctx, first, 100)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(50), active)

	second := base
	second.RequestID = "req-2"
	ok, active, err = store.Place(ctx, second, 100)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(100), active)

	third := base
	third.RequestID = "req-3"
	ok, active, err = store.Place(ctx, third, 100)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, int64(100), active)

	released, err := store.Release(ctx, merchantID, "req-1")
	require.NoError(t, err)
	require.Equal(t, "req-1", released.RequestID)

	ok, active, err = store.Place(ctx, third, 100)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(100), active)
}
