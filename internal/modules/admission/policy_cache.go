package admission

import (
	"sync"
	"time"
)

// DefaultPolicyCacheTTL bounds how long a cached per-trust-level payer spend
// limit may be stale. Long on purpose: the per-trust-level caps are read-mostly
// config — they change only when an admin edits a trust level's caps, never on
// the spend path, and a stale cap is benign (a missing upper bound briefly
// admits a little more, never denies). The COUNTERS that meter spend live in
// Redis and are always live. Pure TTL, no invalidation. Per-invoker GRANTS are
// deliberately NOT cached — they gate whether a delegated invoker may spend at
// all, so a freshly added grant must take effect immediately; the loader reads
// them live (#517).
const DefaultPolicyCacheTTL = 15 * time.Minute

// policyCacheSweepThreshold caps the map: once it grows past this, a miss sweeps
// expired entries (bounds memory without a background goroutine).
const policyCacheSweepThreshold = 8192

type trustLevelCacheEntry struct {
	pol PayerSpendLimits
	exp time.Time
}

// PolicyCache is a process-local, long-TTL cache of the per-trust-level payer
// spend limits (`payer_spend_limits`) — the read-mostly caps, not the live spend
// counters (those stay in Redis) — so the admit hot path skips the trust-level
// policy read on a warm payer. Nil-safe: a nil *PolicyCache reads through every
// time.
type PolicyCache struct {
	ttl         time.Duration
	now         func() time.Time
	mu          sync.Mutex
	trustLevels map[string]trustLevelCacheEntry
}

// NewPolicyCache builds a cache with the given TTL (<=0 uses the default).
func NewPolicyCache(ttl time.Duration) *PolicyCache {
	if ttl <= 0 {
		ttl = DefaultPolicyCacheTTL
	}
	return &PolicyCache{
		ttl:         ttl,
		now:         time.Now,
		trustLevels: make(map[string]trustLevelCacheEntry),
	}
}

// SetClock overrides the clock (tests). Nil-safe.
func (c *PolicyCache) SetClock(now func() time.Time) {
	if c != nil {
		c.now = now
	}
}

// PayerSpendLimits returns the cached per-trust-level payer spend limit for
// (merchant, payer, trustLevel), loading via load() on a miss. Nil receiver
// reads through.
func (c *PolicyCache) PayerSpendLimits(merchant, payer, trustLevel string, load func() (PayerSpendLimits, error)) (PayerSpendLimits, error) {
	if c == nil {
		return load()
	}
	key := merchant + "|" + payer + "|" + trustLevel
	now := c.now()

	c.mu.Lock()
	if e, ok := c.trustLevels[key]; ok && now.Before(e.exp) {
		c.mu.Unlock()
		return e.pol, nil
	}
	c.mu.Unlock()

	pol, err := load()
	if err != nil {
		return PayerSpendLimits{}, err
	}

	c.mu.Lock()
	if len(c.trustLevels) >= policyCacheSweepThreshold {
		for k, e := range c.trustLevels {
			if !now.Before(e.exp) {
				delete(c.trustLevels, k)
			}
		}
	}
	c.trustLevels[key] = trustLevelCacheEntry{pol: pol, exp: now.Add(c.ttl)}
	c.mu.Unlock()
	return pol, nil
}
