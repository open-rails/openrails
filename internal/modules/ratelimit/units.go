package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Weighted reservation pool (#472 G2). This generalizes the count-based
// concurrency primitive (concurrency.go, weight 1) to an arbitrary unit WEIGHT:
// AcquireUnits holds `amount` units of a key's pool while a job is pending and
// ReleaseUnits frees them on completion; admit when the in-flight sum would stay
// within max, deny otherwise. It is the throughput-denominated analogue of a
// money hold (Reserve -> Capture/Release): a batch/queue limit is "N tokens (or
// N requests) may be IN QUEUE at once", not a time-window counter.
//
// Pools live at rlu:<key>, a keyspace distinct from rl:* (fixed windows) and
// rlc:* (count concurrency). Acquire/decrement is one atomic Lua op (portable
// across Redis + Garnet), mirroring concurrency.go. A safety TTL auto-releases a
// crashed caller's hold so a wedged reservation self-heals.

// unitsKey is the Redis key holding the in-flight unit sum for one pool.
func unitsKey(key string) string { return fmt.Sprintf("rlu:%s", key) }

// acquireUnitsScript atomically: reads the current in-flight sum; if admitting
// `amount` more would exceed max, returns {0, current} (not acquired); else
// INCRBYs by amount, (re)sets PEXPIRE = ttl as a crash-safety auto-release, and
// returns {1, newSum}.
var acquireUnitsScript = redis.NewScript(`
local max = tonumber(ARGV[1])
local amt = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
local cur = tonumber(redis.call('GET', KEYS[1]) or '0')
if cur + amt > max then
  return {0, cur}
end
local newv = redis.call('INCRBY', KEYS[1], amt)
redis.call('PEXPIRE', KEYS[1], ttl)
return {1, newv}
`)

// releaseUnitsScript atomically DECRBYs by amount but never below 0: deletes the
// key when the sum reaches <= 0 (so an idle pool leaves no residue).
var releaseUnitsScript = redis.NewScript(`
local amt = tonumber(ARGV[1])
local cur = tonumber(redis.call('GET', KEYS[1]) or '0')
if cur <= 0 then
  redis.call('DEL', KEYS[1])
  return 0
end
local newv = redis.call('DECRBY', KEYS[1], amt)
if newv <= 0 then
  redis.call('DEL', KEYS[1])
  newv = 0
end
return newv
`)

// AcquireUnits tries to hold `amount` units of key's pool, allowing at most max
// in-flight units. On success returns acquired=true and the new in-flight sum;
// when the cap would be exceeded returns acquired=false and the current sum,
// consuming nothing. Every successful acquire (re)sets ttl as a crash-safety
// auto-release. The caller must ReleaseUnits the same amount exactly once per
// successful acquire (at settlement). amount<=0 is a no-op acquire.
func (l *Limiter) AcquireUnits(ctx context.Context, key string, amount, max int64, ttl time.Duration) (acquired bool, current int64, err error) {
	if amount <= 0 {
		cur, cerr := l.UnitsCount(ctx, key)
		return true, cur, cerr
	}
	ttlMs := int64(ttl / time.Millisecond)
	if ttlMs <= 0 {
		ttlMs = 1
	}
	raw, err := acquireUnitsScript.Run(ctx, l.rdb, []string{unitsKey(key)}, max, amount, ttlMs).Result()
	if err != nil {
		return false, 0, err
	}
	vals, ok := raw.([]interface{})
	if !ok || len(vals) < 2 {
		return false, 0, fmt.Errorf("ratelimit: unexpected acquire-units script result %T", raw)
	}
	return toInt64(vals[0]) == 1, toInt64(vals[1]), nil
}

// ReleaseUnits frees `amount` units of key's pool, flooring at 0 so a double
// release (or a release after the safety TTL already expired the pool) can never
// drive the sum negative. amount<=0 is a no-op.
func (l *Limiter) ReleaseUnits(ctx context.Context, key string, amount int64) error {
	if amount <= 0 {
		return nil
	}
	return releaseUnitsScript.Run(ctx, l.rdb, []string{unitsKey(key)}, amount).Err()
}

// UnitsCount returns the current in-flight unit sum for key (introspection).
func (l *Limiter) UnitsCount(ctx context.Context, key string) (int64, error) {
	n, err := l.rdb.Get(ctx, unitsKey(key)).Int64()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return n, nil
}

// QueueLimit is one batch/queue reservation-pool cap (#472 G2): hold at most Max
// units of Unit in-flight per pool. NO time window.
type QueueLimit struct {
	Unit string
	Max  int64
}

// QueueDecision is the outcome of AcquireQueue.
type QueueDecision struct {
	Allowed     bool
	BlockedUnit string // the queue unit whose pool would overflow
	Current     int64  // in-flight sum of the blocking pool
	Max         int64  // cap of the blocking pool
}

// requestQueueKey records the per-request queue holds so settlement can release
// the same pool amounts by request coords ALONE (no base needed at settle time —
// the pool key is stored INSIDE the record). The record stores one hash field
// per held pool key (the full "rlu" pool subkey) -> amount.
func requestQueueKey(source, sourceID string) string {
	return fmt.Sprintf("rlqr:%s:%s", source, sourceID)
}

// AcquireQueue acquires every queue limit's units for one request against the
// per-(payer, endpoint-release) pools under base (#472 G2). It is all-or-nothing:
// if ANY pool would overflow it acquires NOTHING and returns the blocking unit.
// On success it records each held (poolKey -> amount) at a request-scoped key
// (keyed by (source, sourceID) ALONE, TTL = ttl) so ReleaseQueueByRequest can
// free exactly those pools at settlement WITHOUT needing the base again.
//
// amounts supplies each unit's weight for THIS request (e.g. {"request":1} or
// {"token":1500}); a queue limit whose unit is absent/zero in amounts holds 0.
// Idempotent on (source, sourceID): a replay returns allowed without
// re-acquiring (the request-scoped record already exists).
func (l *Limiter) AcquireQueue(ctx context.Context, base, source, sourceID string, limits []QueueLimit, amounts map[string]int64, ttl time.Duration) (QueueDecision, error) {
	if len(limits) == 0 {
		return QueueDecision{Allowed: true}, nil
	}
	reqKey := requestQueueKey(source, sourceID)
	// Idempotency: a replay (record present) is allowed without re-acquiring.
	if n, err := l.rdb.Exists(ctx, reqKey).Result(); err == nil && n > 0 {
		return QueueDecision{Allowed: true}, nil
	}

	type held struct {
		pool   string
		amount int64
	}
	var acquired []held
	rollback := func() {
		for _, h := range acquired {
			_ = l.ReleaseUnits(ctx, h.pool, h.amount)
		}
	}
	for _, ql := range limits {
		amt := amounts[ql.Unit]
		if amt <= 0 {
			continue
		}
		pool := base + ":" + ql.Unit
		ok, cur, err := l.AcquireUnits(ctx, pool, amt, ql.Max, ttl)
		if err != nil {
			rollback()
			return QueueDecision{}, err
		}
		if !ok {
			rollback()
			return QueueDecision{Allowed: false, BlockedUnit: ql.Unit, Current: cur, Max: ql.Max}, nil
		}
		acquired = append(acquired, held{pool: pool, amount: amt})
	}
	// Record the holds for settlement (idempotent release by request coords).
	if len(acquired) > 0 {
		fields := make(map[string]interface{}, len(acquired))
		for _, h := range acquired {
			fields[h.pool] = h.amount
		}
		if err := l.rdb.HSet(ctx, reqKey, fields).Err(); err != nil {
			rollback()
			return QueueDecision{}, err
		}
		// Safety TTL so a crashed settlement can't leak the request record forever.
		_ = l.rdb.PExpire(ctx, reqKey, ttl).Err()
	}
	return QueueDecision{Allowed: true}, nil
}

// ReleaseQueueByRequest frees the queue units a request acquired (#472 G2c),
// looking them up by the request-scoped record (keyed on (source, sourceID)
// alone) and DECRBYing each recorded pool. Idempotent: the record is deleted
// after release, so a second call is a no-op (a completed/failed job frees its
// queued units exactly once).
func (l *Limiter) ReleaseQueueByRequest(ctx context.Context, source, sourceID string) error {
	reqKey := requestQueueKey(source, sourceID)
	holds, err := l.rdb.HGetAll(ctx, reqKey).Result()
	if err != nil {
		return err
	}
	if len(holds) == 0 {
		return nil
	}
	for pool, amtStr := range holds {
		var amt int64
		fmt.Sscan(amtStr, &amt)
		if amt > 0 {
			if rerr := l.ReleaseUnits(ctx, pool, amt); rerr != nil {
				return rerr
			}
		}
	}
	return l.rdb.Del(ctx, reqKey).Err()
}
