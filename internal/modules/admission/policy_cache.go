package admission

import (
	"sync"
	"time"
)

// DefaultPolicyCacheTTL bounds how long a cached spend-cap CONFIG read may be
// stale. Long on purpose: the policy (per-tier caps + delegated-spend caps) is
// read-mostly config — it changes only when an admin/payer edits a cap, never on
// the spend path. So a cap edit simply takes effect within the TTL; there's no
// over-admission risk from stale config (the COUNTERS that meter spend live in
// Redis and are always live). Like the balance cache: pure TTL, no invalidation.
const DefaultPolicyCacheTTL = 10 * time.Minute

// policyCacheSweepThreshold caps each map: once it grows past this, a miss sweeps
// expired entries (bounds memory without a background goroutine).
const policyCacheSweepThreshold = 8192

type tierCacheEntry struct {
	pol TierSpendCaps
	exp time.Time
}

type scopeCacheEntry struct {
	pols []ScopedSpendCap
	exp  time.Time
}

// PolicyCache is a process-local, long-TTL cache of the admit POLICY config: the
// per-tier spend-cap windows (`tier_spend_caps`) and the hierarchical
// delegated-spend caps (`scoped_spend_caps`). It caches the read-mostly CONFIG —
// not the live spend counters (those stay in Redis) — so the admit hot path can
// skip both config reads on a warm payer. Nil-safe: a nil *PolicyCache reads
// through every time.
type PolicyCache struct {
	ttl    time.Duration
	now    func() time.Time
	mu     sync.Mutex
	tiers  map[string]tierCacheEntry
	scopes map[string]scopeCacheEntry
}

// NewPolicyCache builds a cache with the given TTL (<=0 uses the default).
func NewPolicyCache(ttl time.Duration) *PolicyCache {
	if ttl <= 0 {
		ttl = DefaultPolicyCacheTTL
	}
	return &PolicyCache{
		ttl:    ttl,
		now:    time.Now,
		tiers:  make(map[string]tierCacheEntry),
		scopes: make(map[string]scopeCacheEntry),
	}
}

// SetClock overrides the clock (tests). Nil-safe.
func (c *PolicyCache) SetClock(now func() time.Time) {
	if c != nil {
		c.now = now
	}
}

// TierSpendCaps returns the cached per-tier spend-cap policy for (merchant, payer,
// tier), loading via load() on a miss. Nil receiver reads through.
func (c *PolicyCache) TierSpendCaps(merchant, payer, tier string, load func() (TierSpendCaps, error)) (TierSpendCaps, error) {
	if c == nil {
		return load()
	}
	key := merchant + "|" + payer + "|" + tier
	now := c.now()

	c.mu.Lock()
	if e, ok := c.tiers[key]; ok && now.Before(e.exp) {
		c.mu.Unlock()
		return e.pol, nil
	}
	c.mu.Unlock()

	pol, err := load()
	if err != nil {
		return TierSpendCaps{}, err
	}

	c.mu.Lock()
	if len(c.tiers) >= policyCacheSweepThreshold {
		for k, e := range c.tiers {
			if !now.Before(e.exp) {
				delete(c.tiers, k)
			}
		}
	}
	c.tiers[key] = tierCacheEntry{pol: pol, exp: now.Add(c.ttl)}
	c.mu.Unlock()
	return pol, nil
}

// ScopedSpendCaps returns the cached delegated-spend caps for (merchant, payer),
// loading via load() on a miss. Nil receiver reads through.
func (c *PolicyCache) ScopedSpendCaps(merchant, payer string, load func() ([]ScopedSpendCap, error)) ([]ScopedSpendCap, error) {
	if c == nil {
		return load()
	}
	key := merchant + "|" + payer
	now := c.now()

	c.mu.Lock()
	if e, ok := c.scopes[key]; ok && now.Before(e.exp) {
		c.mu.Unlock()
		return e.pols, nil
	}
	c.mu.Unlock()

	pols, err := load()
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if len(c.scopes) >= policyCacheSweepThreshold {
		for k, e := range c.scopes {
			if !now.Before(e.exp) {
				delete(c.scopes, k)
			}
		}
	}
	c.scopes[key] = scopeCacheEntry{pols: pols, exp: now.Add(c.ttl)}
	c.mu.Unlock()
	return pols, nil
}
