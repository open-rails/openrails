package openrails

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/merchant"
)

// EntitlementCheckCacheOptions configures the remote Go client's opt-in cache.
type EntitlementCheckCacheOptions struct {
	// MaxAge bounds the age of an answer, including unscheduled access or
	// credential changes. Scheduled access changes can expire it sooner.
	// Zero disables caching. Only handles with a static APIKey cache checks.
	MaxAge time.Duration
	// Redis optionally shares the cache among replicas using the same API
	// key. Nil uses a bounded in-process cache. Supply trusted Redis 6.2+
	// with synchronized node clocks, as needed for Redis key expiration
	// across failover. Use short client I/O timeouts and ContextTimeoutEnabled
	// to release timed-out cache probes promptly. OpenRails does not close it.
	Redis redis.UniversalClient
}

const (
	entitlementCheckPath          = "/v1/app/entitlements/check"
	entitlementCacheKeyPrefix     = "openrails:entitlement-check:v1:"
	entitlementCacheMaxBytes      = 8 << 20
	entitlementCacheEntryOverhead = 160
	entitlementCacheRedisTimeout  = 25 * time.Millisecond
	entitlementCacheRedisPending  = 64
)

// entitlementDeadlineAhead also checks the wall clock: the monotonic clock can
// stop while a host is suspended, which must not stretch a cached answer.
func entitlementDeadlineAhead(deadline time.Time) bool {
	now := time.Now()
	return now.Before(deadline) && now.Round(0).Before(deadline.Round(0))
}

func (e entitlementCacheEntry) size() int {
	return len(e.key) + len(e.body) + entitlementCacheEntryOverhead
}

type entitlementCacheEntry struct {
	key      string
	body     []byte
	deadline time.Time
}

type entitlementCheckCache struct {
	maxAge time.Duration
	redis  redis.UniversalClient
	probes chan struct{}

	mu      sync.Mutex
	entries map[string]*list.Element
	recency list.List
	bytes   int
}

func newEntitlementCheckCache(opts EntitlementCheckCacheOptions) (*entitlementCheckCache, error) {
	switch {
	case opts.MaxAge < 0:
		return nil, fmt.Errorf("openrails: EntitlementCheckCache.MaxAge must be non-negative")
	case opts.MaxAge == 0:
		return nil, nil
	}
	cache := &entitlementCheckCache{maxAge: opts.MaxAge, redis: opts.Redis}
	if opts.Redis == nil {
		cache.entries = make(map[string]*list.Element)
	} else {
		cache.probes = make(chan struct{}, entitlementCacheRedisPending)
	}
	return cache, nil
}

// cacheableEntitlementKeys mirrors the exact-key query's set semantics. Other
// requests go live, including invalid keys and prefix settings, so the cache
// cannot suppress a server validation error.
func cacheableEntitlementKeys(params billing.CheckEntitlementsParams) []string {
	if !params.At.IsZero() || len(params.Prefixes) != 0 || params.PrefixLimit != 0 {
		return nil
	}
	for _, key := range params.Entitlements {
		if len(key) > catalog.MaxEntitlementKeyBytes || !utf8.ValidString(key) || strings.ContainsRune(key, 0) {
			return nil
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(params.Entitlements)))
}

func entitlementDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (c *Client) entitlementCacheKey(mid billing.MerchantID, customer billing.CustomerID, keys []string) string {
	// A replica must never use an answer cached with a less strict MaxAge.
	parts := []string{c.baseURL, mid.String(), c.entitlementCacheScope, customer.String(), c.entitlementChecks.maxAge.String()}
	raw, _ := json.Marshal(append(parts, keys...))
	return entitlementCacheKeyPrefix + entitlementDigest(raw)
}

// checkEntitlementsCached answers from the cache when it can; live skips the
// lookup and its answer replaces the entry, so a fresh grant is seen at once.
func (c *Client) checkEntitlementsCached(ctx context.Context, params billing.CheckEntitlementsParams, keys []string, live bool) (*billing.EntitlementCheck, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	cache := c.entitlementChecks
	var key string
	var clock entitlementRedisClock
	if mid := c.MerchantID(); !mid.IsZero() && ctx.Err() == nil {
		key = c.entitlementCacheKey(mid, params.CustomerID, keys)
		entry, sample := cache.lookup(ctx, key)
		clock = sample
		var cached billing.EntitlementCheck
		if !live && entry.body != nil && decodeJSON(entry.body, &cached) == nil && entitlementDeadlineAhead(entry.deadline) && ctx.Err() == nil {
			return normalizeEntitlementCheck(&cached), nil
		}
	}

	// The server samples the query time during this request. Starting the
	// relative lifetime before sending it avoids assuming synchronized clocks.
	started := time.Now()
	params.IncludeValidity = true
	response, err := c.doResponse(ctx, http.MethodPost, entitlementCheckPath, params, nil)
	if err != nil {
		return nil, err
	}
	var out billing.EntitlementCheck
	if err := decodeJSON(response.body, &out); err != nil {
		return nil, err
	}
	deadline := entitlementCheckDeadline(started, cache.maxAge, out)
	// doResponse checks the header against the handle when it exists. Require
	// it for caching so only an explicitly bound response can populate entries.
	if mid := c.MerchantID(); !mid.IsZero() && response.header.Get(merchant.BoundHeader) != "" && entitlementDeadlineAhead(deadline) && ctx.Err() == nil {
		if key == "" {
			key = c.entitlementCacheKey(mid, params.CustomerID, keys)
			// The first authenticated reply may have just resolved this handle.
			if cache.redis != nil {
				_, clock = cache.lookup(ctx, key)
			}
		}
		cache.store(ctx, entitlementCacheEntry{key: key, body: response.body, deadline: deadline}, clock, live)
	}
	return normalizeEntitlementCheck(&out), nil
}

func entitlementCheckDeadline(started time.Time, maxAge time.Duration, out billing.EntitlementCheck) time.Time {
	if out.EvaluatedAt == nil || out.EvaluatedAt.IsZero() {
		return time.Time{}
	}
	lifetime := maxAge
	if out.ValidUntil != nil {
		lifetime = min(lifetime, out.ValidUntil.Sub(*out.EvaluatedAt))
	}
	if lifetime <= 0 {
		return time.Time{}
	}
	return started.Add(lifetime)
}

// The payload and lifetime must come from the same Redis entry. TIME lets
// writes translate the local deadline to this key's Redis clock, including
// for a cluster where another node may have a different wall clock.
var entitlementRedisLookup = redis.NewScript(`
local now = redis.call('TIME')
local value = redis.call('GET', KEYS[1])
local ttl = redis.call('PTTL', KEYS[1])
return {tonumber(now[1]), tonumber(now[2]), ttl, value}
`)

type entitlementRedisClock struct {
	server   time.Time
	received time.Time
}

func (c *entitlementCheckCache) lookup(ctx context.Context, key string) (entitlementCacheEntry, entitlementRedisClock) {
	if c.redis != nil {
		return c.redisLookup(ctx, key)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element := c.entries[key]
	if element == nil {
		return entitlementCacheEntry{}, entitlementRedisClock{}
	}
	entry := element.Value.(entitlementCacheEntry)
	if !entitlementDeadlineAhead(entry.deadline) {
		c.remove(element)
		return entitlementCacheEntry{}, entitlementRedisClock{}
	}
	c.recency.MoveToFront(element)
	return entry, entitlementRedisClock{}
}

func (c *entitlementCheckCache) redisLookup(ctx context.Context, key string) (entitlementCacheEntry, entitlementRedisClock) {
	started := time.Now()
	reply := withEntitlementRedisBudget(ctx, c.probes, func(ctx context.Context) []any {
		values, err := entitlementRedisLookup.Run(ctx, c.redis, []string{key}).Slice()
		if err != nil {
			return nil
		}
		return values
	})
	received := time.Now()
	if len(reply) != 4 {
		return entitlementCacheEntry{}, entitlementRedisClock{}
	}
	second, okSecond := reply[0].(int64)
	micro, okMicro := reply[1].(int64)
	if !okSecond || !okMicro {
		return entitlementCacheEntry{}, entitlementRedisClock{}
	}
	clock := entitlementRedisClock{server: time.Unix(second, micro*int64(time.Microsecond)), received: received}
	ttl, ok := reply[2].(int64)
	body, okBody := reply[3].(string)
	if !ok || !okBody || ttl <= 1 || ttl > c.maxAge.Milliseconds() || len(body) > entitlementCacheMaxBytes {
		return entitlementCacheEntry{}, clock
	}
	// Drop a millisecond for Redis clock rounding. Using request start also
	// subtracts network delay, so delayed replies cannot extend access.
	deadline := started.Add(time.Duration(ttl-1) * time.Millisecond)
	return entitlementCacheEntry{key: key, body: []byte(body), deadline: deadline}, clock
}

// store keeps a live entry unless replace is set: an ordinary fill never
// overwrites a valid answer, while a live check's fresher answer does.
func (c *entitlementCheckCache) store(ctx context.Context, entry entitlementCacheEntry, clock entitlementRedisClock, replace bool) {
	if c.redis != nil {
		if clock.received.IsZero() || time.Until(entry.deadline) < time.Millisecond || !entitlementDeadlineAhead(entry.deadline.Add(-time.Millisecond)) {
			return
		}
		// PXAT prevents a slow SET or a client retry from starting a fresh
		// lifetime. The Redis sample was taken before Go received it.
		expiry := clock.server.Add(entry.deadline.Sub(clock.received))
		args := redis.SetArgs{ExpireAt: expiry, Mode: "NX"}
		if replace {
			args.Mode = ""
		}
		withEntitlementRedisBudget(ctx, c.probes, func(ctx context.Context) error {
			return c.redis.SetArgs(ctx, entry.key, entry.body, args).Err()
		})
		return
	}
	if entry.size() > entitlementCacheMaxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !entitlementDeadlineAhead(entry.deadline) {
		return
	}
	if previous := c.entries[entry.key]; previous != nil {
		if !replace && entitlementDeadlineAhead(previous.Value.(entitlementCacheEntry).deadline) {
			return
		}
		c.remove(previous)
	}
	for c.bytes+entry.size() > entitlementCacheMaxBytes {
		c.remove(c.recency.Back())
	}
	c.entries[entry.key] = c.recency.PushFront(entry)
	c.bytes += entry.size()
}

func (c *entitlementCheckCache) remove(element *list.Element) {
	entry := element.Value.(entitlementCacheEntry)
	delete(c.entries, entry.key)
	c.recency.Remove(element)
	c.bytes -= entry.size()
}

// Merchant-supplied Redis clients may ignore context deadlines on socket I/O.
// Limit both the wait per request and the work still running after timeout.
func withEntitlementRedisBudget[T any](ctx context.Context, pending chan struct{}, op func(context.Context) T) (zero T) {
	ctx, cancel := context.WithTimeout(ctx, entitlementCacheRedisTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return zero
	}
	select {
	case pending <- struct{}{}:
	default:
		return zero
	}
	result := make(chan T, 1)
	go func() {
		defer func() { <-pending }()
		result <- op(ctx)
	}()
	select {
	case value := <-result:
		return value
	case <-ctx.Done():
		return zero
	}
}
