// Package abusestate keeps abuse state: rate-limit windows, admin lockouts,
// captcha challenges and card-testing declines. It lives in Redis when one is configured, which
// every instance shares, and otherwise in this process's memory, which serves
// one instance only. A configured Redis that stops answering is left for
// memory until it answers again. Never PostgreSQL.
package abusestate

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
)

// tableLimit bounds each memory table; a full table drops the entry closest
// to expiry.
const tableLimit = 100_000

// errNotReached is a configured Redis before its first answer.
var errNotReached = errors.New("not reached yet")

// Store is one process's abuse state. Its methods never fail a request: what
// Redis cannot take, memory does.
type Store struct {
	rdb *redis.Client
	now func() time.Time

	counts, holds, marks *table

	mu       sync.Mutex
	observed bool
	err      error // the last Redis failure; nil while it answers
	down     atomic.Bool

	fallbacks atomic.Int64
}

// New is the store over rdb; nil keeps everything in memory.
func New(rdb *redis.Client) *Store {
	return &Store{
		rdb: rdb, now: time.Now,
		counts: newTable(tableLimit), holds: newTable(tableLimit), marks: newTable(tableLimit),
	}
}

// SetClock replaces the clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Count adds n to key's current window, a clock-aligned span of window, and
// returns the window's count and the time until it ends.
func (s *Store) Count(ctx context.Context, key string, n int64, window time.Duration) (int64, time.Duration) {
	now := s.now()
	k, end := windowKey(key, window, now)
	ttl := end.Sub(now)
	if s.redis() {
		pipe := s.rdb.TxPipeline()
		pipe.SetNX(ctx, k, 0, ttl)
		incr := pipe.IncrBy(ctx, k, n)
		_, err := pipe.Exec(ctx)
		if err == nil {
			return incr.Val(), ttl
		}
		s.fail(ctx, err)
	}
	return s.counts.add(k, n, end, now), ttl
}

// ResetCounts ends keys' current windows of window.
func (s *Store) ResetCounts(ctx context.Context, window time.Duration, keys ...string) error {
	now := s.now()
	ks := make([]string, len(keys))
	for i, key := range keys {
		ks[i], _ = windowKey(key, window, now)
	}
	return s.release(ctx, s.counts, ks)
}

// Hold holds key for ttl: a lockout or a captcha challenge.
func (s *Store) Hold(ctx context.Context, key string, ttl time.Duration) {
	now := s.now()
	if s.redis() {
		err := s.rdb.Set(ctx, key, 1, ttl).Err()
		if err == nil {
			return
		}
		s.fail(ctx, err)
	}
	s.holds.set(key, now.Add(ttl), now)
}

// Held is how long key is still held; zero when it is not. A hold memory took
// while Redis was down still counts once it answers again.
func (s *Store) Held(ctx context.Context, key string) time.Duration {
	left := s.holds.left(key, s.now())
	if s.redis() {
		d, err := s.rdb.PTTL(ctx, key).Result()
		if err != nil {
			s.fail(ctx, err)
		} else if d > left {
			left = d
		}
	}
	return left
}

// Release ends keys' holds. Memory releases them at once; the error is a
// configured Redis that did not.
func (s *Store) Release(ctx context.Context, keys ...string) error {
	return s.release(ctx, s.holds, keys)
}

// A Mark puts Member in Set until TTL passes; marking it again restarts it.
type Mark struct {
	Set, Member string
	TTL         time.Duration
}

// Mark records marks at now, the caller's clock.
func (s *Store) Mark(ctx context.Context, now time.Time, marks ...Mark) {
	if s.redis() {
		pipe := s.rdb.Pipeline()
		for _, m := range marks {
			pipe.ZAdd(ctx, m.Set, redis.Z{Score: float64(now.Add(m.TTL).UnixMilli()), Member: m.Member})
			pipe.ZRemRangeByScore(ctx, m.Set, "-inf", strconv.FormatInt(now.UnixMilli(), 10))
			pipe.PExpire(ctx, m.Set, m.TTL)
		}
		_, err := pipe.Exec(ctx)
		if err == nil {
			return
		}
		s.fail(ctx, err)
	}
	for _, m := range marks {
		s.marks.mark(m.Set, m.Member, now.Add(m.TTL), now)
	}
}

// Marked is how many members each set holds at now, the caller's clock: in
// Redis, and in memory what was marked while Redis was down.
func (s *Store) Marked(ctx context.Context, now time.Time, sets ...string) []int64 {
	out := make([]int64, len(sets))
	for i, set := range sets {
		out[i] = int64(s.marks.members(set, now))
	}
	if s.redis() {
		pipe := s.rdb.Pipeline()
		live := "(" + strconv.FormatInt(now.UnixMilli(), 10)
		counts := make([]*redis.IntCmd, len(sets))
		for i, set := range sets {
			counts[i] = pipe.ZCount(ctx, set, live, "+inf")
		}
		if _, err := pipe.Exec(ctx); err != nil {
			s.fail(ctx, err)
			return out
		}
		for i, c := range counts {
			out[i] += c.Val()
		}
	}
	return out
}

func (s *Store) release(ctx context.Context, t *table, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	t.remove(keys)
	if s.rdb == nil {
		return nil
	}
	if s.down.Load() {
		return s.RedisErr()
	}
	if err := s.rdb.Del(ctx, keys...).Err(); err != nil {
		s.fail(ctx, err)
		return err
	}
	return nil
}

// Probe asks Redis whether it answers and records the answer. Nothing to ask
// without Redis.
func (s *Store) Probe(ctx context.Context) error {
	if s.rdb == nil {
		return nil
	}
	err := s.rdb.Ping(ctx).Err()
	if ctx.Err() != nil {
		return err
	}
	s.record(err)
	return err
}

// UsesRedis reports whether a Redis is configured.
func (s *Store) UsesRedis() bool { return s != nil && s.rdb != nil }

// RedisErr is why the configured Redis is not in use; nil while it answers.
func (s *Store) RedisErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.observed {
		return errNotReached
	}
	return s.err
}

// Fallbacks counts the operations memory took because the configured Redis
// did not answer.
func (s *Store) Fallbacks() int64 { return s.fallbacks.Load() }

// redis reports whether to ask Redis; false also counts a fallback.
func (s *Store) redis() bool {
	if s.rdb == nil {
		return false
	}
	if s.down.Load() {
		s.fallbacks.Add(1)
		return false
	}
	return true
}

// fail leaves Redis for memory until Probe hears it again. A request that
// ended is not Redis failing.
func (s *Store) fail(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	s.fallbacks.Add(1)
	s.record(err)
}

func (s *Store) record(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	first, wasUp := !s.observed, s.err == nil
	s.observed, s.err = true, err
	s.down.Store(err != nil)
	switch {
	case err != nil && (first || wasUp):
		log.WithError(err).Error("redis: unreachable; abuse state is in this process's memory until it answers")
	case err == nil && !first && !wasUp:
		log.Info("redis: reachable")
	}
}

// windowKey is key's window of window at now, and when it ends.
func windowKey(key string, window time.Duration, now time.Time) (string, time.Time) {
	seconds := max(int64(window/time.Second), 1)
	index := now.Unix() / seconds
	return key + ":" + strconv.FormatInt(seconds, 10) + ":" + strconv.FormatInt(index, 10), time.Unix((index+1)*seconds, 0)
}
