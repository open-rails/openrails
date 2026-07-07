package copilot

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/modules/ratelimit"
)

// Per-merchant catalog-ask budget (mirrors #756's dashboard.AskLimiter
// doctrine exactly, own Redis key namespace — "catalog-ask:" — so the two
// features never share a budget). Every ask is up to askMaxTurns LLM
// requests, so this is the deployment's LLM spend guard for this surface.
const (
	askRatePerMinute = 10
	askRatePerDay    = 200
)

var askRatePolicy = ratelimit.Policy{Windows: []ratelimit.Limit{
	{Unit: "request", Window: time.Minute, Max: askRatePerMinute},
	{Unit: "request", Window: 24 * time.Hour, Max: askRatePerDay},
}}

// AskLimiter rate-limits catalog Q&A/drafting per merchant. retryAfter is
// meaningful only when allowed is false.
type AskLimiter interface {
	AllowAsk(ctx context.Context, merchantID string) (allowed bool, retryAfter time.Duration, err error)
}

// NewAskLimiter builds the production limiter: Redis-backed fixed windows
// when configured, else an in-process fallback with the same windows.
func NewAskLimiter(rdb redis.Cmdable, now func() time.Time) AskLimiter {
	if now == nil {
		now = time.Now
	}
	if rdb == nil {
		return &memoryAskLimiter{now: now, counters: map[string]*memoryAskCounter{}}
	}
	l := ratelimit.NewLimiter(rdb)
	l.SetClock(now)
	return &redisAskLimiter{limiter: l}
}

type redisAskLimiter struct{ limiter *ratelimit.Limiter }

func (r *redisAskLimiter) AllowAsk(ctx context.Context, merchantID string) (bool, time.Duration, error) {
	dec, err := r.limiter.Check(ctx, "catalog-ask:"+merchantID, askRatePolicy, map[string]int64{"request": 1})
	if err != nil {
		// Redis down: allow (rate limiting is defense-in-depth; the ask loop's
		// own caps bound per-request cost) but say so.
		log.WithError(err).Warn("catalog ask rate limiter unavailable; allowing request")
		return true, 0, nil
	}
	if dec.Allowed {
		return true, 0, nil
	}
	var retry time.Duration
	for _, w := range dec.Windows {
		if w.Remaining <= 0 && w.ResetAfter > retry {
			retry = w.ResetAfter
		}
	}
	if retry <= 0 {
		retry = time.Minute
	}
	return false, retry, nil
}

// memoryAskLimiter is the no-Redis fallback: same fixed windows, one process.
type memoryAskLimiter struct {
	mu       sync.Mutex
	now      func() time.Time
	counters map[string]*memoryAskCounter
}

type memoryAskCounter struct {
	bucket int64
	count  int64
}

func (m *memoryAskLimiter) AllowAsk(_ context.Context, merchantID string) (bool, time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()

	type slot struct {
		c     *memoryAskCounter
		reset time.Duration
	}
	slots := make([]slot, 0, len(askRatePolicy.Windows))
	var retry time.Duration
	blocked := false
	for _, w := range askRatePolicy.Windows {
		secs := int64(w.Window / time.Second)
		bucket := now.Unix() / secs
		key := fmt.Sprintf("%s:%d", merchantID, secs)
		c, ok := m.counters[key]
		if !ok || c.bucket != bucket {
			c = &memoryAskCounter{bucket: bucket}
			m.counters[key] = c
		}
		reset := time.Duration(secs-(now.Unix()%secs)) * time.Second
		if c.count+1 > w.Max {
			blocked = true
			if reset > retry {
				retry = reset
			}
		}
		slots = append(slots, slot{c: c, reset: reset})
	}
	if blocked {
		return false, retry, nil
	}
	for _, s := range slots {
		s.c.count++
	}
	return true, 0, nil
}
