package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Concurrency is the CONCURRENCY (in-flight) axis of admission control, distinct
// from the fixed-window throughput axis in limiter.go: it caps how many requests
// may be simultaneously in flight per key (issue #298), not how many per window.
//
// A slot is acquired before work starts and released when it ends. Because a
// caller can crash mid-request (and never release), every acquire (re)sets a
// safety TTL on the counter so a wedged slot eventually self-heals rather than
// leaking forever. Release floors at 0 so a double-release or a release after a
// TTL-expiry can never drive the count negative.
//
// Counters live at rlc:<key>, separate from the rl:* throughput keyspace. The
// read+increment (or decrement) is one atomic server-side op via Lua, portable
// across Redis and Garnet, mirroring checkScript in limiter.go.

// concurrencyKey is the Redis key holding the in-flight count for one bucket.
func concurrencyKey(key string) string { return fmt.Sprintf("rlc:%s", key) }

// acquireConcurrencyScript atomically: reads the current in-flight count; if
// admitting one more would exceed max, returns {0, current} (not acquired);
// else INCRs, (re)sets PEXPIRE = ttl as a crash-safety auto-release, and returns
// {1, newCount}. The TTL is always refreshed on a successful acquire so an
// actively-used slot never expires out from under a live caller.
var acquireConcurrencyScript = redis.NewScript(`
local max = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
local cur = tonumber(redis.call('GET', KEYS[1]) or '0')
if cur + 1 > max then
  return {0, cur}
end
local newv = redis.call('INCR', KEYS[1])
redis.call('PEXPIRE', KEYS[1], ttl)
return {1, newv}
`)

// releaseConcurrencyScript atomically DECRs but never below 0: if the count is
// already <= 0 it deletes the key and returns 0; otherwise it DECRs, deletes the
// key when it reaches 0 (so an idle bucket leaves no residue), and returns the
// new count.
var releaseConcurrencyScript = redis.NewScript(`
local cur = tonumber(redis.call('GET', KEYS[1]) or '0')
if cur <= 0 then
  redis.call('DEL', KEYS[1])
  return 0
end
local newv = redis.call('DECR', KEYS[1])
if newv <= 0 then
  redis.call('DEL', KEYS[1])
  newv = 0
end
return newv
`)

// AcquireConcurrency tries to claim one in-flight slot for key, allowing at most
// max concurrent holders. On success it returns acquired=true and the new count
// (including this acquire); when the cap is already reached it returns
// acquired=false and the current count, consuming nothing. Every successful
// acquire (re)sets ttl on the counter as a crash-safety auto-release. The caller
// must ReleaseConcurrency exactly once per successful acquire.
func (l *Limiter) AcquireConcurrency(ctx context.Context, key string, max int64, ttl time.Duration) (acquired bool, current int64, err error) {
	ttlMs := int64(ttl / time.Millisecond)
	if ttlMs <= 0 {
		ttlMs = 1
	}
	raw, err := acquireConcurrencyScript.Run(ctx, l.rdb, []string{concurrencyKey(key)}, max, ttlMs).Result()
	if err != nil {
		return false, 0, err
	}
	vals, ok := raw.([]interface{})
	if !ok || len(vals) < 2 {
		return false, 0, fmt.Errorf("ratelimit: unexpected acquire script result %T", raw)
	}
	return toInt64(vals[0]) == 1, toInt64(vals[1]), nil
}

// ReleaseConcurrency frees one in-flight slot for key, flooring at 0 so a double
// release (or a release after the safety TTL already expired the counter) can
// never drive the count negative.
func (l *Limiter) ReleaseConcurrency(ctx context.Context, key string) error {
	return releaseConcurrencyScript.Run(ctx, l.rdb, []string{concurrencyKey(key)}).Err()
}

// ConcurrencyCount returns the current in-flight count for key (introspection).
func (l *Limiter) ConcurrencyCount(ctx context.Context, key string) (int64, error) {
	n, err := l.rdb.Get(ctx, concurrencyKey(key)).Int64()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return n, nil
}
