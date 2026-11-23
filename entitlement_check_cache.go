package openrails

import (
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// EntitlementCheckCacheOptions configures the remote Go client's entitlement
// check cache, which is on by default for handles with a static APIKey. It
// keeps each allowed key's answer, never a denial: a check is answered from
// the cache only when every requested key is allowed there. A newly held key
// is therefore seen at once, while a change to a held key, such as its seats
// or a revocation, can take up to MaxAge unless CheckEntitlementsLive sees it.
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
const DefaultEntitlementCheckMaxAge = 2 * time.Minute

const (
	entitlementCheckPath          = "/v1/app/entitlements/check"
	entitlementCacheKeyPrefix     = "openrails:entitlement-check:v2:"
	entitlementCacheMaxBytes      = 8 << 20
	entitlementCacheEntryOverhead = 256
	entitlementCacheRedisTimeout  = 25 * time.Millisecond
	entitlementCacheEvictTimeout  = 250 * time.Millisecond
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
	// generation, when set, makes this a scope's record of its last live
	// denial rather than a key's answer.
	generation uint64
}

func (e entitlementCacheEntry) size() int {
	return len(e.key) + entitlementCacheEntryOverhead
}

// entitlementCacheScope names one customer's entries and the generation a
// live denial advances, so a fill begun before that denial is refused.
type entitlementCacheScope struct {
	generation string
	keys       []string
}

// entitlementCacheState is what a fill read before its request was sent.
// Without it an answer can only evict.
type entitlementCacheState struct {
	ok         bool
	epoch      uint64
	generation string
	clock      entitlementRedisClock
}

type entitlementCheckCache struct {
	maxAge    time.Duration
	redis     redis.UniversalClient
	probes    chan struct{}
	evictions chan struct{}

	// epoch numbers in-process live denials; a fill that read an older epoch
	// than its scope's record, or than floor, is refused.
	epoch   atomic.Uint64
	mu      sync.Mutex
	floor   uint64
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
		cache.evictions = make(chan struct{}, entitlementCacheRedisPending)
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

// entitlementCacheScopeOf names the scope's keys. The hash tag keeps them in
// one Redis Cluster slot, so one script reads or writes them all.
func (c *Client) entitlementCacheScopeOf(mid billing.MerchantID, customer billing.CustomerID, keys []string) entitlementCacheScope {
	// A replica must never use an answer cached with a less strict MaxAge.
	raw, _ := json.Marshal([]string{c.baseURL, mid.String(), c.entitlementCacheScope, customer.String(), c.entitlementChecks.maxAge.String()})
	scope := entitlementCacheScope{generation: entitlementCacheKeyPrefix + "{" + entitlementDigest(raw) + "}", keys: make([]string, len(keys))}
	for i, key := range keys {
		scope.keys[i] = scope.generation + ":" + key
	}
	return scope
}

// checkEntitlementsCached answers from the cache when every key has a live
// allowed entry; live skips the lookup. Each answer from OpenRails stores its
// allowed keys and drops its denied ones. A live denial also advances the
// scope's generation, so a fill begun before it cannot restore the entry.
func (c *Client) checkEntitlementsCached(ctx context.Context, params billing.CheckEntitlementsParams, keys []string, live bool) (*billing.EntitlementCheck, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	cache := c.entitlementChecks
	scope := c.entitlementCacheScopeOf(c.merchantID, params.CustomerID, keys)
	var state entitlementCacheState
	if ctx.Err() == nil {
		if live {
			state = cache.probe(ctx, scope)
		} else {
			var entries []entitlementCacheEntry
			entries, state = cache.lookup(ctx, scope)
			// A delayed reply or a pause since the lookup can outlast an entry.
			if entries != nil && entitlementEntriesAhead(entries) && ctx.Err() == nil {
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
	deadline := entitlementCheckDeadline(started, cache.maxAge, out)
	storable := state.ok && entitlementDeadlineAhead(deadline) && ctx.Err() == nil
	var allowed []entitlementCacheEntry
	var denied []string
	for i, key := range keys {
		switch {
		case !out.Entitlements[key]:
			denied = append(denied, scope.keys[i])
		case storable:
			allowed = append(allowed, entitlementCacheEntry{key: scope.keys[i], value: entitlementCacheValue{
				Quantity:    copyPointer(out.Quantities[key]),
				EvaluatedAt: *out.EvaluatedAt,
				ValidUntil:  copyPointer(out.ValidUntil),
			}, deadline: deadline})
		}
	}
	cache.record(ctx, scope, state, allowed, denied, live)
	return normalizeEntitlementCheck(&out), nil
}

func entitlementEntriesAhead(entries []entitlementCacheEntry) bool {
	for _, entry := range entries {
		if !entitlementDeadlineAhead(entry.deadline) {
			return false
		}
	}
	return true
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

// KEYS[1] is the scope's generation, the rest its entries. Each value and
// lifetime come from the same entry. TIME lets writes translate the local
// deadline to this slot's clock, which another cluster node may not share.
var entitlementRedisLookup = redis.NewScript(`
local now = redis.call('TIME')
local reply = {tonumber(now[1]), tonumber(now[2]), redis.call('GET', KEYS[1]) or ''}
for i = 2, #KEYS do
  reply[2 * i] = redis.call('PTTL', KEYS[i])
  reply[2 * i + 1] = redis.call('GET', KEYS[i])
end
return reply
`)

// entitlementRedisProbe reads only the clock and generation, for a live check.
var entitlementRedisProbe = redis.NewScript(`
local now = redis.call('TIME')
return {tonumber(now[1]), tonumber(now[2]), redis.call('GET', KEYS[1]) or ''}
`)

// KEYS[1] is the scope's generation; the next ARGV[1] keys are set to
// ARGV[7..] and the rest dropped. Drops and a new generation (ARGV[3]) always
// apply; sets apply only if the generation is still the one read before the
// request (ARGV[2]), with PXAT ARGV[5] and NX unless ARGV[6] is empty.
var entitlementRedisFill = redis.NewScript(`
local sets = tonumber(ARGV[1])
local current = redis.call('GET', KEYS[1]) or ''
for i = sets + 2, #KEYS do
  redis.call('DEL', KEYS[i])
end
if ARGV[3] ~= '' then
  redis.call('SET', KEYS[1], ARGV[3], 'PX', ARGV[4])
end
if current ~= ARGV[2] then
  return 0
end
for i = 1, sets do
  if ARGV[6] == '' then
    redis.call('SET', KEYS[i + 1], ARGV[6 + i], 'PXAT', ARGV[5])
  else
    redis.call('SET', KEYS[i + 1], ARGV[6 + i], 'PXAT', ARGV[5], 'NX')
  end
end
return 1
`)

type entitlementRedisClock struct {
	server   time.Time
	received time.Time
}

// lookup returns one live entry per key, or nil when any is missing, and the
// state a fill needs.
func (c *entitlementCheckCache) lookup(ctx context.Context, scope entitlementCacheScope) ([]entitlementCacheEntry, entitlementCacheState) {
	if c.redis != nil {
		return c.redisLookup(ctx, scope)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state := entitlementCacheState{ok: true, epoch: c.epoch.Load()}
	entries := make([]entitlementCacheEntry, len(scope.keys))
	hit := true
	for i, key := range scope.keys {
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
		return nil, state
	}
	return entries, state
}

// probe reads the state a live check's fill needs, without its entries.
func (c *entitlementCheckCache) probe(ctx context.Context, scope entitlementCacheScope) entitlementCacheState {
	if c.redis == nil {
		return entitlementCacheState{ok: true, epoch: c.epoch.Load()}
	}
	reply, received := c.redisRun(ctx, entitlementRedisProbe, []string{scope.generation})
	if len(reply) != 3 {
		return entitlementCacheState{}
	}
	return redisCacheState(reply, received)
}

func (c *entitlementCheckCache) redisRun(ctx context.Context, script *redis.Script, keys []string) ([]any, time.Time) {
	reply := withEntitlementRedisBudget(ctx, ctx, c.probes, entitlementCacheRedisTimeout, func(ctx context.Context) []any {
		values, err := script.Run(ctx, c.redis, keys).Slice()
		if err != nil {
			return nil
		}
		return values
	})
	return reply, time.Now()
}

// redisCacheState reads the clock and generation leading a script's reply.
func redisCacheState(reply []any, received time.Time) entitlementCacheState {
	second, okSecond := reply[0].(int64)
	micro, okMicro := reply[1].(int64)
	generation, okGeneration := reply[2].(string)
	if !okSecond || !okMicro || !okGeneration {
		return entitlementCacheState{}
	}
	return entitlementCacheState{ok: true, generation: generation, clock: entitlementRedisClock{server: time.Unix(second, micro*int64(time.Microsecond)), received: received}}
}

func (c *entitlementCheckCache) redisLookup(ctx context.Context, scope entitlementCacheScope) ([]entitlementCacheEntry, entitlementCacheState) {
	started := time.Now()
	reply, received := c.redisRun(ctx, entitlementRedisLookup, append([]string{scope.generation}, scope.keys...))
	if len(reply) != 3+2*len(scope.keys) {
		return nil, entitlementCacheState{}
	}
	state := redisCacheState(reply, received)
	if !state.ok {
		return nil, state
	}
	entries := make([]entitlementCacheEntry, len(scope.keys))
	for i, key := range scope.keys {
		ttl, okTTL := reply[3+2*i].(int64)
		raw, okRaw := reply[4+2*i].(string)
		if !okTTL || !okRaw || ttl <= 1 || ttl > c.maxAge.Milliseconds() {
			return nil, state
		}
		var value entitlementCacheValue
		if json.Unmarshal([]byte(raw), &value) != nil || value.EvaluatedAt.IsZero() {
			return nil, state
		}
		// Drop a millisecond for Redis clock rounding. Using request start also
		// subtracts network delay, so delayed replies cannot extend access.
		entries[i] = entitlementCacheEntry{key: key, value: value, deadline: started.Add(time.Duration(ttl-1) * time.Millisecond)}
	}
	return entries, state
}

// record drops denied keys' entries and stores allowed ones, refusing the
// stores when a live denial has advanced the scope's generation since state
// was read. Unless live, a store keeps a valid entry: an ordinary fill never
// overwrites one, while a live check's fresher answer does.
func (c *entitlementCheckCache) record(ctx context.Context, scope entitlementCacheScope, state entitlementCacheState, allowed []entitlementCacheEntry, denied []string, live bool) {
	if c.redis != nil {
		c.redisRecord(ctx, scope, state, allowed, denied, live)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	stale := !state.ok || state.epoch < c.floor
	if element := c.entries[scope.generation]; element != nil && element.Value.(entitlementCacheEntry).generation > state.epoch {
		stale = true
	}
	for _, key := range denied {
		if element := c.entries[key]; element != nil {
			c.remove(element)
		}
	}
	if live && len(denied) != 0 {
		c.push(entitlementCacheEntry{key: scope.generation, generation: c.epoch.Add(1), deadline: time.Now().Add(2 * c.maxAge)})
	}
	if stale {
		return
	}
	for _, entry := range allowed {
		if !entitlementDeadlineAhead(entry.deadline) {
			continue
		}
		if previous := c.entries[entry.key]; !live && previous != nil && entitlementDeadlineAhead(previous.Value.(entitlementCacheEntry).deadline) {
			continue
		}
		c.push(entry)
	}
}

// push replaces entry.key's element, evicting the least recently used to stay
// within the byte budget. Evicting a generation record raises the floor, so
// fills it would have refused stay refused. It needs c.mu held.
func (c *entitlementCheckCache) push(entry entitlementCacheEntry) {
	if previous := c.entries[entry.key]; previous != nil {
		c.remove(previous)
	}
	for c.bytes+entry.size() > entitlementCacheMaxBytes {
		oldest := c.recency.Back()
		c.floor = max(c.floor, oldest.Value.(entitlementCacheEntry).generation)
		c.remove(oldest)
	}
	c.entries[entry.key] = c.recency.PushFront(entry)
	c.bytes += entry.size()
}

func (c *entitlementCheckCache) redisRecord(ctx context.Context, scope entitlementCacheScope, state entitlementCacheState, allowed []entitlementCacheEntry, denied []string, live bool) {
	keys := []string{scope.generation}
	var values []any
	// One check's entries share one deadline.
	if state.ok && len(allowed) != 0 && time.Until(allowed[0].deadline) >= time.Millisecond && entitlementDeadlineAhead(allowed[0].deadline.Add(-time.Millisecond)) {
		for _, entry := range allowed {
			value, err := json.Marshal(entry.value)
			if err != nil {
				return
			}
			keys = append(keys, entry.key)
			values = append(values, value)
		}
	}
	var bump string
	if live && len(denied) != 0 {
		token := make([]byte, 16)
		_, _ = rand.Read(token)
		bump = hex.EncodeToString(token)
	}
	if len(values) == 0 && len(denied) == 0 {
		return
	}
	keys = append(keys, denied...)
	// PXAT prevents a slow SET or a client retry from starting a fresh
	// lifetime. The Redis sample was taken before Go received it.
	var expiry int64
	if len(values) != 0 {
		expiry = state.clock.server.Add(allowed[0].deadline.Sub(state.clock.received)).UnixMilli()
	}
	mode := "NX"
	if live {
		mode = ""
	}
	args := append([]any{len(values), state.generation, bump, max(2*c.maxAge.Milliseconds(), 1), expiry, mode}, values...)
	write := func(ctx context.Context) error {
		return entitlementRedisFill.Run(ctx, c.redis, keys, args...).Err()
	}
	if bump == "" {
		withEntitlementRedisBudget(ctx, ctx, c.probes, entitlementCacheRedisTimeout, write)
		return
	}
	// A live denial's eviction has its own budget, so lookups cannot crowd it
	// out. It outlives the caller's context, which only bounds the wait.
	withEntitlementRedisBudget(ctx, context.WithoutCancel(ctx), c.evictions, entitlementCacheEvictTimeout, write)
}

func (c *entitlementCheckCache) remove(element *list.Element) {
	entry := element.Value.(entitlementCacheEntry)
	delete(c.entries, entry.key)
	c.recency.Remove(element)
	c.bytes -= entry.size()
}

// Merchant-supplied Redis clients may ignore context deadlines on socket I/O.
// Limit both the wait per request and the work still running after timeout.
// op runs under run bounded by timeout; the caller also stops waiting when
// wait is done, leaving op to finish within its own bound.
func withEntitlementRedisBudget[T any](wait, run context.Context, pending chan struct{}, timeout time.Duration, op func(context.Context) T) (zero T) {
	run, cancel := context.WithTimeout(run, timeout)
	if run.Err() != nil {
		cancel()
		return zero
	}
	select {
	case pending <- struct{}{}:
	default:
		cancel()
		return zero
	}
	result := make(chan T, 1)
	go func() {
		defer func() { <-pending }()
		defer cancel()
		result <- op(run)
	}()
	select {
	case value := <-result:
		return value
	case <-run.Done():
		return zero
	case <-wait.Done():
		return zero
	}
}
