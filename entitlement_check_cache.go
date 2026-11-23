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

// EntitlementCheckCacheOptions configures the remote Go client's entitlement
// check cache, which is on by default for handles with a static APIKey. It
// keeps each allowed key's answer, never a denial: a check is answered from
// the cache only when every requested key is allowed there, so a purchase is
// seen at once.
type EntitlementCheckCacheOptions struct {
	// MaxAge bounds the age of an allowed answer, including unscheduled
	// changes such as a revocation, a refund or the API key's permissions.
	// Scheduled access changes expire it sooner. Zero is
	// DefaultEntitlementCheckMaxAge.
	MaxAge time.Duration
	// Disabled turns the cache off: every check asks OpenRails.
	Disabled bool
	// Redis optionally shares the cache among replicas using the same API
	// key. Nil uses a bounded in-process cache. Supply trusted Redis 6.2+
	// with synchronized node clocks, as needed for Redis key expiration
	// across failover. Use short client I/O timeouts and ContextTimeoutEnabled
	// to release timed-out cache probes promptly. OpenRails does not close it.
	Redis redis.UniversalClient
}

// DefaultEntitlementCheckMaxAge is how long an allowed answer is reused when
// EntitlementCheckCacheOptions.MaxAge is zero.
const DefaultEntitlementCheckMaxAge = 10 * time.Second

const (
	entitlementCheckPath          = "/v1/app/entitlements/check"
	entitlementCacheKeyPrefix     = "openrails:entitlement-check:v2:"
	entitlementCacheMaxBytes      = 8 << 20
	entitlementCacheEntryOverhead = 256
	entitlementCacheRedisTimeout  = 25 * time.Millisecond
	entitlementCacheRedisPending  = 64
)

// entitlementDeadlineAhead also checks the wall clock: the monotonic clock can
// stop while a host is suspended, which must not stretch a cached answer.
func entitlementDeadlineAhead(deadline time.Time) bool {
	now := time.Now()
	return now.Before(deadline) && now.Round(0).Before(deadline.Round(0))
}

// entitlementCacheValue is one allowed key's answer, with the metadata of the
// check that produced it.
type entitlementCacheValue struct {
	Quantity    *int       `json:"quantity"`
	EvaluatedAt time.Time  `json:"evaluated_at"`
	ValidUntil  *time.Time `json:"valid_until"`
}

type entitlementCacheEntry struct {
	key      string
	value    entitlementCacheValue
	deadline time.Time
}

func (e entitlementCacheEntry) size() int {
	return len(e.key) + entitlementCacheEntryOverhead
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
	case opts.Disabled:
		return nil, nil
	case opts.MaxAge == 0:
		opts.MaxAge = DefaultEntitlementCheckMaxAge
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

// entitlementCacheKeys names each key's entry. The hash tag over the scope
// keeps one customer's entries in one Redis Cluster slot, so one script reads
// them all.
func (c *Client) entitlementCacheKeys(mid billing.MerchantID, customer billing.CustomerID, keys []string) []string {
	// A replica must never use an answer cached with a less strict MaxAge.
	raw, _ := json.Marshal([]string{c.baseURL, mid.String(), c.entitlementCacheScope, customer.String(), c.entitlementChecks.maxAge.String()})
	tag := entitlementCacheKeyPrefix + "{" + entitlementDigest(raw) + "}:"
	out := make([]string, len(keys))
	for i, key := range keys {
		out[i] = tag + key
	}
	return out
}

// checkEntitlementsCached answers from the cache when every key has a live
// allowed entry; live skips the lookup. Each answer from OpenRails stores its
// allowed keys and drops its denied ones, so a revocation a live check sees
// is not served again from the cache.
func (c *Client) checkEntitlementsCached(ctx context.Context, params billing.CheckEntitlementsParams, keys []string, live bool) (*billing.EntitlementCheck, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	cache := c.entitlementChecks
	var cacheKeys []string
	var clock entitlementRedisClock
	if mid := c.MerchantID(); !mid.IsZero() && ctx.Err() == nil {
		cacheKeys = c.entitlementCacheKeys(mid, params.CustomerID, keys)
		// A live check still samples the Redis clock for its write.
		if !live || cache.redis != nil {
			entries, sample := cache.lookup(ctx, cacheKeys)
			clock = sample
			if !live && entries != nil && ctx.Err() == nil {
				return composeEntitlementCheck(keys, entries), nil
			}
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
	if cacheKeys == nil {
		// The first authenticated reply may have just resolved this handle.
		mid := c.MerchantID()
		if mid.IsZero() {
			return normalizeEntitlementCheck(&out), nil
		}
		cacheKeys = c.entitlementCacheKeys(mid, params.CustomerID, keys)
		if cache.redis != nil {
			_, clock = cache.lookup(ctx, cacheKeys)
		}
	}
	deadline := entitlementCheckDeadline(started, cache.maxAge, out)
	// doResponse checks the header against the handle when it exists. Require
	// it for storing so only an explicitly bound response can populate entries.
	storable := response.header.Get(merchant.BoundHeader) != "" && entitlementDeadlineAhead(deadline) && ctx.Err() == nil
	var allowed []entitlementCacheEntry
	var denied []string
	for i, key := range keys {
		switch {
		case !out.Entitlements[key]:
			denied = append(denied, cacheKeys[i])
		case storable:
			allowed = append(allowed, entitlementCacheEntry{key: cacheKeys[i], value: entitlementCacheValue{
				Quantity:    copyPointer(out.Quantities[key]),
				EvaluatedAt: *out.EvaluatedAt,
				ValidUntil:  copyPointer(out.ValidUntil),
			}, deadline: deadline})
		}
	}
	cache.record(ctx, allowed, denied, clock, live)
	return normalizeEntitlementCheck(&out), nil
}

func copyPointer[T any](p *T) *T {
	if p == nil {
		return nil
	}
	return new(*p)
}

// composeEntitlementCheck answers every key from its allowed entry. The answer
// is as old as its oldest entry and valid until its earliest boundary.
func composeEntitlementCheck(keys []string, entries []entitlementCacheEntry) *billing.EntitlementCheck {
	out := billing.EntitlementCheck{Entitlements: make(map[string]bool, len(keys)), Quantities: make(map[string]*int, len(keys))}
	for i, entry := range entries {
		out.Entitlements[keys[i]] = true
		out.Quantities[keys[i]] = copyPointer(entry.value.Quantity)
		if out.EvaluatedAt == nil || entry.value.EvaluatedAt.Before(*out.EvaluatedAt) {
			out.EvaluatedAt = new(entry.value.EvaluatedAt)
		}
		if until := entry.value.ValidUntil; until != nil && (out.ValidUntil == nil || until.Before(*out.ValidUntil)) {
			out.ValidUntil = new(*until)
		}
	}
	return normalizeEntitlementCheck(&out)
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

// Each value and lifetime must come from the same Redis entry. TIME lets
// writes translate the local deadline to this slot's Redis clock, including
// for a cluster where another node may have a different wall clock.
var entitlementRedisLookup = redis.NewScript(`
local now = redis.call('TIME')
local reply = {tonumber(now[1]), tonumber(now[2])}
for i, key in ipairs(KEYS) do
  reply[2 * i + 1] = redis.call('PTTL', key)
  reply[2 * i + 2] = redis.call('GET', key)
end
return reply
`)

type entitlementRedisClock struct {
	server   time.Time
	received time.Time
}

// lookup returns one live entry per key, or nil when any is missing.
func (c *entitlementCheckCache) lookup(ctx context.Context, keys []string) ([]entitlementCacheEntry, entitlementRedisClock) {
	if c.redis != nil {
		return c.redisLookup(ctx, keys)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entries := make([]entitlementCacheEntry, len(keys))
	hit := true
	for i, key := range keys {
		element := c.entries[key]
		if element == nil {
			hit = false
			continue
		}
		entry := element.Value.(entitlementCacheEntry)
		if !entitlementDeadlineAhead(entry.deadline) {
			c.remove(element)
			hit = false
			continue
		}
		c.recency.MoveToFront(element)
		entries[i] = entry
	}
	if !hit {
		return nil, entitlementRedisClock{}
	}
	return entries, entitlementRedisClock{}
}

func (c *entitlementCheckCache) redisLookup(ctx context.Context, keys []string) ([]entitlementCacheEntry, entitlementRedisClock) {
	started := time.Now()
	reply := withEntitlementRedisBudget(ctx, c.probes, func(ctx context.Context) []any {
		values, err := entitlementRedisLookup.Run(ctx, c.redis, keys).Slice()
		if err != nil {
			return nil
		}
		return values
	})
	received := time.Now()
	if len(reply) != 2+2*len(keys) {
		return nil, entitlementRedisClock{}
	}
	second, okSecond := reply[0].(int64)
	micro, okMicro := reply[1].(int64)
	if !okSecond || !okMicro {
		return nil, entitlementRedisClock{}
	}
	clock := entitlementRedisClock{server: time.Unix(second, micro*int64(time.Microsecond)), received: received}
	entries := make([]entitlementCacheEntry, len(keys))
	for i, key := range keys {
		ttl, okTTL := reply[2+2*i].(int64)
		raw, okRaw := reply[3+2*i].(string)
		if !okTTL || !okRaw || ttl <= 1 || ttl > c.maxAge.Milliseconds() {
			return nil, clock
		}
		var value entitlementCacheValue
		if json.Unmarshal([]byte(raw), &value) != nil || value.EvaluatedAt.IsZero() {
			return nil, clock
		}
		// Drop a millisecond for Redis clock rounding. Using request start also
		// subtracts network delay, so delayed replies cannot extend access.
		entries[i] = entitlementCacheEntry{key: key, value: value, deadline: started.Add(time.Duration(ttl-1) * time.Millisecond)}
	}
	return entries, clock
}

// record stores allowed entries and drops denied keys' entries. Unless replace
// is set, a store keeps a live entry: an ordinary fill never overwrites a
// valid answer, while a live check's fresher answer does.
func (c *entitlementCheckCache) record(ctx context.Context, allowed []entitlementCacheEntry, denied []string, clock entitlementRedisClock, replace bool) {
	if c.redis != nil {
		c.redisRecord(ctx, allowed, denied, clock, replace)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, key := range denied {
		if element := c.entries[key]; element != nil {
			c.remove(element)
		}
	}
	for _, entry := range allowed {
		c.store(entry, replace)
	}
}

// store needs c.mu held.
func (c *entitlementCheckCache) store(entry entitlementCacheEntry, replace bool) {
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

// redisRecord writes in one pipeline: every key shares one cluster slot.
func (c *entitlementCheckCache) redisRecord(ctx context.Context, allowed []entitlementCacheEntry, denied []string, clock entitlementRedisClock, replace bool) {
	type write struct {
		key   string
		value []byte
		args  redis.SetArgs
	}
	writes := make([]write, 0, len(allowed))
	for _, entry := range allowed {
		if clock.received.IsZero() || time.Until(entry.deadline) < time.Millisecond || !entitlementDeadlineAhead(entry.deadline.Add(-time.Millisecond)) {
			continue
		}
		value, err := json.Marshal(entry.value)
		if err != nil {
			continue
		}
		// PXAT prevents a slow SET or a client retry from starting a fresh
		// lifetime. The Redis sample was taken before Go received it.
		args := redis.SetArgs{ExpireAt: clock.server.Add(entry.deadline.Sub(clock.received)), Mode: "NX"}
		if replace {
			args.Mode = ""
		}
		writes = append(writes, write{key: entry.key, value: value, args: args})
	}
	if len(writes) == 0 && len(denied) == 0 {
		return
	}
	withEntitlementRedisBudget(ctx, c.probes, func(ctx context.Context) error {
		_, err := c.redis.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, w := range writes {
				pipe.SetArgs(ctx, w.key, w.value, w.args)
			}
			if len(denied) != 0 {
				pipe.Del(ctx, denied...)
			}
			return nil
		})
		return err
	})
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
