package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Usage-value windows are the generic "accumulate a $-valued quantity per fixed
// window, then read the running total" primitive (#488 wasted-spend), built on
// the SAME rl:* fixed-window keyspace + bucketing as Check — but DECOUPLED into
// two ops: ADD (record value, never denies) and READ (current total, no
// mutation). Throughput's Check folds record+deny into one atomic call; a
// wasted-spend report and the admit-time budget check happen at different times,
// so they can't share one call.

// AddWindowValue adds value to the fixed window (base, unit, window) and returns
// the window's new running total. A no-op (returns 0) for value <= 0. The bucket
// + key derivation matches Check exactly, so the same (base, unit, window) is one
// shared counter across record + read.
func (l *Limiter) AddWindowValue(ctx context.Context, base, unit string, window time.Duration, value int64) (int64, error) {
	if value <= 0 {
		return 0, nil
	}
	secs := int64(window / time.Second)
	if secs <= 0 {
		secs = 1
	}
	now := l.now().UTC()
	bucket := now.Unix() / secs
	key := fmt.Sprintf("rl:%s:%s:%d:%d", base, unit, secs, bucket)
	pipe := l.rdb.TxPipeline()
	incr := pipe.IncrBy(ctx, key, value)
	pipe.PExpire(ctx, key, time.Duration(secs)*time.Second+time.Second) // window + 1s slack
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return incr.Val(), nil
}

// WindowValue reads the current running total of the fixed window (base, unit,
// window) WITHOUT mutating it (0 when the bucket is empty/expired).
func (l *Limiter) WindowValue(ctx context.Context, base, unit string, window time.Duration) (int64, error) {
	secs := int64(window / time.Second)
	if secs <= 0 {
		secs = 1
	}
	now := l.now().UTC()
	bucket := now.Unix() / secs
	key := fmt.Sprintf("rl:%s:%s:%d:%d", base, unit, secs, bucket)
	n, err := l.rdb.Get(ctx, key).Int64()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return n, nil
}

// or#903 deleted ClaimOnce, the SetNX "short-lived idempotency key". Its only
// caller was the wasted-spend report claim, and a cache key that expires and
// does not survive a flush is not an idempotency claim — it only looked like
// one. Durable once-only belongs to a keyed money write; do not reintroduce
// this primitive to give some other path the same illusion.
