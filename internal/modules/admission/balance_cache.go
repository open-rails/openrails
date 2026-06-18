package admission

import (
	"sync"
	"time"
)

// DefaultBalanceCacheTTL bounds how long a cached affordability figure may be
// stale. The only staleness it admits is SETTLED-balance changes
// (deposits/captures/withdraws) — in-flight holds are shared via the Redis
// `held` gauge, NOT this cache, so the gate always sees live in-flight. There is
// deliberately NO explicit invalidation: a debit not yet refreshed is bounded
// OVER-admission (accepted, #512 decision 8) and a deposit becomes spendable
// within the window — every balance change simply rides the TTL.
const DefaultBalanceCacheTTL = 10 * time.Second

// balCacheSweepThreshold caps the map: once it grows past this, a miss sweeps
// expired entries (bounds memory without a background goroutine).
const balCacheSweepThreshold = 8192

type capEntry struct {
	available  int64
	creditLine int64
	exp        time.Time
}

// BalanceCache is a process-local, short-TTL cache of the admit affordability
// inputs (available balance + arrears credit line) per (merchant, customer,
// currency). It removes the per-admit ledger-balance AGGREGATION read from the
// hot path for hot payers (the balance is a SUM over the customer's transfers;
// the other admit reads are point lookups). Nil-safe: a nil *BalanceCache always
// reads through, so callers without a cache (and tests) need no special-casing.
type BalanceCache struct {
	ttl   time.Duration
	now   func() time.Time
	mu    sync.Mutex
	items map[string]capEntry
}

// NewBalanceCache builds a cache with the given TTL (<=0 uses the default).
func NewBalanceCache(ttl time.Duration) *BalanceCache {
	if ttl <= 0 {
		ttl = DefaultBalanceCacheTTL
	}
	return &BalanceCache{ttl: ttl, now: time.Now, items: make(map[string]capEntry)}
}

// SetClock overrides the clock (tests). Nil-safe.
func (c *BalanceCache) SetClock(now func() time.Time) {
	if c != nil {
		c.now = now
	}
}

func balCacheKey(merchant, customer, currency string) string {
	return merchant + "|" + customer + "|" + currency
}

// Capacity returns the (available, creditLine) for one payer, served from cache
// when fresh and otherwise loaded via load() and cached. A nil receiver reads
// through every time (no caching). load is the authoritative read (payerCapacity).
func (c *BalanceCache) Capacity(merchant, customer, currency string, load func() (available, creditLine int64, err error)) (int64, int64, error) {
	if c == nil {
		return load()
	}
	key := balCacheKey(merchant, customer, currency)
	now := c.now()

	c.mu.Lock()
	if e, ok := c.items[key]; ok && now.Before(e.exp) {
		c.mu.Unlock()
		return e.available, e.creditLine, nil
	}
	c.mu.Unlock()

	available, creditLine, err := load()
	if err != nil {
		return 0, 0, err
	}

	c.mu.Lock()
	if len(c.items) >= balCacheSweepThreshold {
		for k, e := range c.items {
			if !now.Before(e.exp) {
				delete(c.items, k)
			}
		}
	}
	c.items[key] = capEntry{available: available, creditLine: creditLine, exp: now.Add(c.ttl)}
	c.mu.Unlock()
	return available, creditLine, nil
}
