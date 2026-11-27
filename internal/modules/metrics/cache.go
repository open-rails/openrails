package metrics

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// CacheTTL is how long a result of a plan reading the MRR facts is served
// again: a cancellation at period end counts at most this late.
const CacheTTL = 60 * time.Second

// memoryEntries bounds the process cache: past it, expired entries go, then
// all.
const memoryEntries = 1024

// resultCache holds the results of plans reading the MRR facts, by merchant
// and plan, in Redis when configured and in process memory otherwise. Each
// key computes once at a time: in this process, and across processes while
// one holds its Redis lock.
type resultCache struct {
	rdb redis.UniversalClient
	now func() time.Time

	mu      sync.Mutex
	memory  map[string]cachedResult
	pending map[string]*flight
}

type cachedResult struct {
	At   time.Time
	Body json.RawMessage
}

type flight struct {
	done chan struct{}
	res  *Result
	err  error
}

// CacheMRR caches the results of plans that read the MRR facts for CacheTTL,
// in rdb when set; now is the clock the TTL runs on.
func (s *Service) CacheMRR(rdb redis.UniversalClient, now func() time.Time) {
	s.cache = &resultCache{rdb: rdb, now: now, memory: map[string]cachedResult{}, pending: map[string]*flight{}}
}

// readsMRR reports a plan any of whose measures reads the MRR facts.
func readsMRR(plan *Plan) bool {
	for _, m := range plan.Measures {
		for _, leaf := range m.components() {
			switch leaf.Family {
			case FamMRR, FamMRRFlows, FamMRRBase, FamMRRTrailing, FamScheduled, FamCohort, FamRisk, FamRiskFlows:
				return true
			}
		}
	}
	return false
}

// cacheKey is the schema, the merchant and a hash of what the plan answers:
// its measures, dimensions, filters, grain, range, zone, comparison, order
// and limit; never the instant it is asked at. A restored merchant keeps its
// id in another schema: the schema keeps their results apart.
func cacheKey(schema string, merchantID uuid.UUID, plan *Plan) string {
	names := make([]string, len(plan.Measures))
	for i, m := range plan.Measures {
		names[i] = m.Name
	}
	filters := make([][]string, 0, len(plan.Filters))
	for _, d := range sortedFilterDims(plan.Filters) {
		filters = append(filters, append([]string{d}, plan.Filters[d]...))
	}
	body, _ := json.Marshal(struct {
		Measures []string
		Dims     []string
		Filters  [][]string
		HasTime  bool
		Grain    string
		From, To time.Time
		Zone     string
		Compare  string
		Order    []OrderTerm
		Limit    int
	}{names, plan.Dims, filters, plan.HasTime, plan.Grain, plan.From.UTC(), plan.To.UTC(), plan.zone().String(), plan.Compare, plan.Order, plan.Limit})
	sum := sha256.Sum256(body)
	return "openrails:metrics:" + schema + ":" + merchantID.String() + ":" + hex.EncodeToString(sum[:])
}

// fresh reports an entry computed less than CacheTTL before now.
func (c *resultCache) fresh(e cachedResult) bool {
	age := c.now().Sub(e.At)
	return age >= 0 && age < CacheTTL
}

// get is key's fresh result, if any.
func (c *resultCache) get(ctx context.Context, key string) (*Result, bool) {
	var e cachedResult
	if c.rdb != nil {
		raw, err := c.rdb.Get(ctx, key).Bytes()
		if err != nil || json.Unmarshal(raw, &e) != nil {
			return nil, false
		}
	} else {
		c.mu.Lock()
		e = c.memory[key]
		c.mu.Unlock()
	}
	if e.Body == nil || !c.fresh(e) {
		return nil, false
	}
	// Numbers stay as written: a count is not read back as a float.
	dec := json.NewDecoder(bytes.NewReader(e.Body))
	dec.UseNumber()
	var res Result
	if dec.Decode(&res) != nil {
		return nil, false
	}
	return &res, true
}

// put keeps res under key for CacheTTL; a cache that refuses it costs only
// the next computation.
func (c *resultCache) put(ctx context.Context, key string, res *Result) {
	body, err := json.Marshal(res)
	if err != nil {
		return
	}
	e := cachedResult{At: c.now(), Body: body}
	if c.rdb != nil {
		if raw, err := json.Marshal(e); err == nil {
			_ = c.rdb.Set(ctx, key, raw, CacheTTL).Err()
		}
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.memory) >= memoryEntries {
		for k, old := range c.memory {
			if !c.fresh(old) {
				delete(c.memory, k)
			}
		}
		if len(c.memory) >= memoryEntries {
			c.memory = map[string]cachedResult{}
		}
	}
	c.memory[key] = e
}

// lockPoll is how often a process waiting on another's computation looks.
const lockPoll = 50 * time.Millisecond

// do answers key from the cache, or computes it once: a caller finding the
// key computing in this process waits for it, and with Redis one finding it
// computing elsewhere waits until it lands or the lock lapses. A waiter
// whose computation's caller went away computes it itself.
func (c *resultCache) do(ctx context.Context, key string, compute func(context.Context) (*Result, error)) (*Result, error) {
	if res, ok := c.get(ctx, key); ok {
		return res, nil
	}
	c.mu.Lock()
	if f, ok := c.pending[key]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			if f.err != nil && ctx.Err() == nil && (errors.Is(f.err, context.Canceled) || errors.Is(f.err, context.DeadlineExceeded)) {
				return c.do(ctx, key, compute)
			}
			return f.res, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	c.pending[key] = f
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		close(f.done)
	}()
	f.res, f.err = c.computeOnce(ctx, key, compute)
	return f.res, f.err
}

// computeOnce computes key under its Redis lock when there is one.
func (c *resultCache) computeOnce(ctx context.Context, key string, compute func(context.Context) (*Result, error)) (*Result, error) {
	if c.rdb != nil {
		lock := key + ":computing"
		for {
			taken, err := c.rdb.SetNX(ctx, lock, "1", CacheTTL).Result()
			if err != nil || taken {
				if taken {
					defer c.rdb.Del(context.WithoutCancel(ctx), lock)
				}
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(lockPoll):
			}
			if res, ok := c.get(ctx, key); ok {
				return res, nil
			}
		}
		if res, ok := c.get(ctx, key); ok {
			return res, nil
		}
	}
	res, err := compute(ctx)
	if err != nil {
		return nil, err
	}
	c.put(ctx, key, res)
	return res, nil
}

// cacheable reports whether plan's results are cached for merchantID.
func (s *Service) cacheable(plan *Plan, merchantID uuid.UUID) bool {
	return s.cache != nil && merchantID != uuid.Nil && readsMRR(plan)
}
