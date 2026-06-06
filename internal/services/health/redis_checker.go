package health

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisHealthChecker verifies Redis-compatible Garnet connectivity.
type RedisHealthChecker struct {
	client *redis.Client
	name   string
}

// NewRedisHealthChecker creates a checker for the Redis-compatible cache service.
func NewRedisHealthChecker(client *redis.Client) *RedisHealthChecker {
	return &RedisHealthChecker{client: client, name: "garnet"}
}

func (r *RedisHealthChecker) Name() string           { return r.name }
func (r *RedisHealthChecker) Timeout() time.Duration { return 3 * time.Second }

func (r *RedisHealthChecker) Check(ctx context.Context) error {
	if r == nil || r.client == nil {
		return fmt.Errorf("garnet client is nil")
	}
	pong, err := r.client.Ping(ctx).Result()
	if err != nil {
		return fmt.Errorf("garnet health check failed: %w", err)
	}
	if pong != "PONG" {
		return fmt.Errorf("garnet health check failed: unexpected response %q", pong)
	}
	return nil
}
