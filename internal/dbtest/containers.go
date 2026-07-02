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
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
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
	rdb := NewSharedRedisClient(t)
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.FlushDB(ctx).Err())
	return rdb, ctx
}

// NewSharedRedisClient returns a fresh client over the shared Redis WITHOUT
// binding its lifetime to t and WITHOUT flushing — for package-shared harnesses
// that outlive the creating test. The caller owns Close.
func NewSharedRedisClient(t testing.TB) *redis.Client {
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
	require.NoError(t, rdb.Ping(ctx).Err())
	return rdb
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

var (
	chOnce      sync.Once
	chErr       error
	chContainer *tcclickhouse.ClickHouseContainer
	chHTTPAddr  string
	chNative    string
)

// SharedClickHouse provisions one process-shared ClickHouse for the opt-in
// analytics tests (admin metrics; ClickHouse has no RLS). Honors the
// OPENRAILS_TEST_CH_* env overrides used by scripts/test_integration.sh.
// Returns the HTTP address (http://host:port) and the native TCP address.
func SharedClickHouse(t *testing.T) (httpAddr, nativeAddr string) {
	t.Helper()
	ctx := context.Background()
	chOnce.Do(func() {
		if env := firstEnv("OPENRAILS_TEST_CH_HTTP_ADDR", "OPENRAILS_TEST_CH_HTTP_URL"); env != "" {
			chHTTPAddr = env
			chNative = firstEnv("OPENRAILS_TEST_CH_ADDR", "OPENRAILS_TEST_CH_NATIVE_ADDR")
			if chNative == "" {
				chNative = "localhost:9003"
			}
			return
		}
		chContainer, chErr = tcclickhouse.Run(ctx,
			"clickhouse/clickhouse-server:25.8-alpine",
			tcclickhouse.WithUsername("test_user"),
			tcclickhouse.WithPassword("test_password"),
			tcclickhouse.WithDatabase("test_analytics"),
			WithClickHouseLimits(),
			testcontainers.WithWaitStrategy(
				wait.ForHTTP("/ping").WithPort("8123/tcp").WithStartupTimeout(60*time.Second),
			),
		)
		if chErr != nil {
			chErr = fmt.Errorf("start clickhouse container: %w", chErr)
			return
		}
		host, herr := chContainer.Host(ctx)
		if herr != nil {
			chErr = herr
			return
		}
		httpPort, perr := chContainer.MappedPort(ctx, "8123")
		if perr != nil {
			chErr = perr
			return
		}
		nativePort, perr := chContainer.MappedPort(ctx, "9000")
		if perr != nil {
			chErr = perr
			return
		}
		chHTTPAddr = fmt.Sprintf("http://%s:%s", host, httpPort.Port())
		chNative = fmt.Sprintf("%s:%s", host, nativePort.Port())
	})
	require.NoError(t, chErr, "provision shared integration clickhouse")
	return chHTTPAddr, chNative
}

// ClickHouseEnvOr returns the OPENRAILS_TEST_CH_* env value or the fallback —
// credentials/database for the shared container match its Run options.
func ClickHouseEnvOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// TerminateSharedClickHouse terminates the shared ClickHouse container if this
// process started one. Safe and idempotent.
func TerminateSharedClickHouse() {
	if chContainer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := chContainer.Terminate(ctx); err != nil {
		fmt.Printf("dbtest: failed to terminate shared clickhouse container: %v\n", err)
	}
	chContainer = nil
}
