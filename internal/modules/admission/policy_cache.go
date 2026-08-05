package admission

import (
	"strings"
	"sync"
	"time"
)

// DefaultPolicyCacheTTL bounds how long a cached policy RESOLUTION may be stale.
// Long on purpose: policies and bindings are read-mostly config — they change
// only when a merchant declares or rebinds one, never on the spend path. The
// COUNTERS that meter spend live in Redis and are always live. Per-invoker
// GRANTS are deliberately NOT cached — they gate whether a delegated invoker may
// spend at all, so a freshly added grant must take effect immediately; the
// loader reads them live (#517).
const DefaultPolicyCacheTTL = 15 * time.Minute

// policyCacheSweepThreshold caps the map: once it grows past this, a miss sweeps
// expired entries (bounds memory without a background goroutine).
const policyCacheSweepThreshold = 8192

type policyCacheEntry struct {
	pol ResolvedPolicy
	gen uint64
	exp time.Time
}

// PolicyCache is a process-local cache of the or#897 billing-policy RESOLUTION
// for a (merchant, payer, tier) — the read-mostly binding lookup, not the live
// spend counters (those stay in Redis) — so the admit hot path skips the policy
// read on a warm payer. Nil-safe: a nil *PolicyCache reads through every time.
//
// TTL alone is not enough here, unlike the caps it replaces. A rebinding can
// TIGHTEN a policy (or switch its kind), so serving a stale binding would keep
// admitting under a policy the merchant has already revoked. Writes therefore
// bump a per-merchant generation and every older entry for that merchant is
// dead on read — O(1), no scan.
type PolicyCache struct {
	ttl         time.Duration
	now         func() time.Time
	mu          sync.Mutex
	entries     map[string]policyCacheEntry
	generations map[string]uint64
}

// NewPolicyCache builds a cache with the given TTL (<=0 uses the default).
func NewPolicyCache(ttl time.Duration) *PolicyCache {
	if ttl <= 0 {
		ttl = DefaultPolicyCacheTTL
	}
	return &PolicyCache{
		ttl:         ttl,
		now:         time.Now,
		entries:     make(map[string]policyCacheEntry),
		generations: make(map[string]uint64),
	}
}

// SetClock overrides the clock (tests). Nil-safe.
func (c *PolicyCache) SetClock(now func() time.Time) {
	if c != nil {
		c.now = now
	}
}

// InvalidateMerchant retires every cached resolution for one merchant. Called
// on any policy or binding write, so the next admit re-resolves.
func (c *PolicyCache) InvalidateMerchant(merchant string) {
	if c == nil {
		return
	}
	merchant = strings.TrimSpace(merchant)
	if merchant == "" {
		return
	}
	c.mu.Lock()
	c.generations[merchant]++
	c.mu.Unlock()
}

// ResolvedPolicy returns the cached resolution for (merchant, payer, tier),
// loading via load() on a miss. Nil receiver reads through.
func (c *PolicyCache) ResolvedPolicy(merchant, payer, tier string, load func() (ResolvedPolicy, error)) (ResolvedPolicy, error) {
	if c == nil {
		return load()
	}
	key := merchant + "|" + payer + "|" + tier
	now := c.now()

	c.mu.Lock()
	generation := c.generations[merchant]
	if e, ok := c.entries[key]; ok && e.gen == generation && now.Before(e.exp) {
		c.mu.Unlock()
		return e.pol, nil
	}
	c.mu.Unlock()

	pol, err := load()
	if err != nil {
		return ResolvedPolicy{}, err
	}

	c.mu.Lock()
	if len(c.entries) >= policyCacheSweepThreshold {
		for k, e := range c.entries {
			if !now.Before(e.exp) {
				delete(c.entries, k)
			}
		}
	}
	// A write that landed WHILE load() was in flight may have raced it, so the
	// value we hold could already be stale. Return it (the caller asked for a
	// snapshot) but do not cache it — the next admit re-reads.
	if c.generations[merchant] == generation {
		c.entries[key] = policyCacheEntry{pol: pol, gen: generation, exp: now.Add(c.ttl)}
	}
	c.mu.Unlock()
	return pol, nil
}
