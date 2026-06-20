//go:build integration

package dbtest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

func WithPostgresLimits() testcontainers.ContainerCustomizer {
	return testcontainers.WithHostConfigModifier(PostgresHostConfigModifier)
}

func WithRedisLimits() testcontainers.ContainerCustomizer {
	return testcontainers.WithHostConfigModifier(RedisHostConfigModifier)
}

func WithClickHouseLimits() testcontainers.ContainerCustomizer {
	return testcontainers.WithHostConfigModifier(ClickHouseHostConfigModifier)
}

func PostgresHostConfigModifier(hc *container.HostConfig) {
	setContainerLimits(hc, 2<<30, 2_000_000_000)
}

func RedisHostConfigModifier(hc *container.HostConfig) {
	setContainerLimits(hc, 256<<20, 500_000_000)
}

func ClickHouseHostConfigModifier(hc *container.HostConfig) {
	setContainerLimits(hc, 2<<30, 2_000_000_000)
}

func setContainerLimits(hc *container.HostConfig, memoryBytes, nanoCPUs int64) {
	hc.Resources.Memory = memoryBytes
	hc.Resources.NanoCPUs = nanoCPUs
}

var (
	redisOnce      sync.Once
	redisErr       error
	redisContainer *tcredis.RedisContainer
	redisURL       string
)

func SharedRedisClient(t *testing.T) (*redis.Client, context.Context) {
	t.Helper()
	ctx := context.Background()
	redisOnce.Do(func() {
		if addr := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_REDIS_ADDR")); addr != "" {
			redisURL = "redis://" + addr
			return
		}
		redisContainer, redisErr = tcredis.Run(ctx, "redis:7-alpine", WithRedisLimits())
		if redisErr != nil {
			redisErr = fmt.Errorf("start redis container: %w", redisErr)
			return
		}
		redisURL, redisErr = redisContainer.ConnectionString(ctx)
	})
	require.NoError(t, redisErr, "provision shared integration redis")

	opt, err := redis.ParseURL(redisURL)
	require.NoError(t, err)
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(ctx).Err())
	require.NoError(t, rdb.FlushDB(ctx).Err())
	return rdb, ctx
}

func TerminateSharedRedis() {
	if redisContainer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := redisContainer.Terminate(ctx); err != nil {
		fmt.Printf("dbtest: failed to terminate shared redis container: %v\n", err)
	}
	redisContainer = nil
}
