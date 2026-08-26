package spendgate

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/redis/go-redis/v9"
)

// payerBase is the hash-tagged Redis key prefix for one payer+currency. The
// {merchant:customer} hash tag co-locates every key this payer's scripts touch
// (held gauge, per-request hold record, window counters) on one Cluster
// slot — required because the scripts are multi-key and construct/store window
// keys in Lua.
func payerBase(merchant, customer, currency string) string {
	return fmt.Sprintf("sg:{%s:%s}:%s", merchant, customer, currency)
}

// reqPtrKey maps a (merchant, request id) to the payer coords its hold lives
// under, so capture/release — which arrive with only the request id (the wire the
// consumers already use) — can resolve customer+currency server-side without a
// contract change. Written on admit, deleted on capture/release, TTL'd to the hold.
func reqPtrKey(merchant, requestID string) string {
	return "sg:req:" + merchant + ":" + requestID
}

const reqPtrSep = "\x1f"

// The per-request hold record stored at "<base>:hold:<reqID>" is a "|"-delimited
// string: "<reservedCost>|<windowKey1>|<windowKey2>|...". The leading field is the
// reserved estimate (freed from `held` at capture/release); the remaining fields
// are the EXACT window counter keys this admit incremented, so release can
// decrement precisely the buckets it hit (no policy reload, no roll ambiguity).
// '|' never appears in a key (keys are scope/id/key/bucket joined by ':').
//
// "<base>:holds" is a SET indexing the live hold-record keys (same hash slot), so
// the recompute path can re-derive `held` = Σ live hold costs without SCAN.

// admitScript atomically, all-or-nothing:
//  1. idempotent no-op if this request's hold record already exists (retry safety);
//  2. affordability gate: accountBalance - held - cost >= floor (floor = -creditLimit;
//     prepaid = 0). accountBalance is the caller's ledger snapshot; `held` is the
//     shared in-flight reservation gauge;
//  3. window gate: every applicable window can fit cost in its current bucket;
//  4. iff all pass, RESERVE: held += cost, each window += cost (with a bucket TTL),
//     and store the hold record (cost + window keys).
//
// Windows are PER-USER-STAGGERED by a deterministic phase `offset` derived from the
// (customer-scoped) window key (passed in by the caller) — bucket =
// floor((now-offset)/dur), boundaries at offset + k*dur. No stored anchor: the
// phase is recomputable, so a Redis flush can't desync it and there is no
// first-charge / anchor-TTL bookkeeping.
//
// KEYS: [1]=held [2]=hold:<reqID> [3]=holds index set. ARGV: [1]=cost [2]=floor
// [3]=accountBalance [4]=holdTtlMs [5]=nowMs [6]=n, then per window
// {prefix, durMs, limit, offsetMs}.
// Returns {allowed(1/0), blocked}: blocked 0=ok, -1=affordability, i>0 = window i.
var admitScript = redis.NewScript(`
local cost  = tonumber(ARGV[1])
local floor = tonumber(ARGV[2])
local cbal  = tonumber(ARGV[3])
local holdTtl = tonumber(ARGV[4])
local now = tonumber(ARGV[5])
local n = tonumber(ARGV[6])

if redis.call('EXISTS', KEYS[2]) == 1 then
  return {1, 0}
end

local held = tonumber(redis.call('GET', KEYS[1]) or '0')
if cbal - held - cost < floor then
  -- held may include holds whose records TTL-expired (abandoned admits, #676):
  -- recompute held = sum of LIVE hold records from the index before denying.
  local sum = 0
  local members = redis.call('SMEMBERS', KEYS[3])
  for _, m in ipairs(members) do
    local rec = redis.call('GET', m)
    if rec then
      local sep = string.find(rec, '|', 1, true)
      if sep then sum = sum + (tonumber(string.sub(rec, 1, sep-1)) or 0)
      else sum = sum + (tonumber(rec) or 0) end
    else
      redis.call('SREM', KEYS[3], m)
    end
  end
  if sum > 0 then redis.call('SET', KEYS[1], sum) else redis.call('DEL', KEYS[1]) end
  held = sum
  if cbal - held - cost < floor then
    return {0, -1}
  end
end

-- check phase: no writes
for i=1,n do
  local b = 6 + (i-1)*4
  local prefix = ARGV[b+1]
  local dur    = tonumber(ARGV[b+2])
  local limit  = tonumber(ARGV[b+3])
  local offset = tonumber(ARGV[b+4])
  local bucket = math.floor((now - offset) / dur)
  local cntKey = prefix .. ':' .. string.format('%d', bucket)
  local cur = tonumber(redis.call('GET', cntKey) or '0')
  if cur + cost > limit then
    return {0, i}
  end
end

-- reserve phase: all gates passed
redis.call('INCRBY', KEYS[1], cost)
local rec = tostring(cost)
for i=1,n do
  local b = 6 + (i-1)*4
  local prefix = ARGV[b+1]
  local dur    = tonumber(ARGV[b+2])
  local offset = tonumber(ARGV[b+4])
  local bucket = math.floor((now - offset) / dur)
  local cntKey = prefix .. ':' .. string.format('%d', bucket)
  local newv = redis.call('INCRBY', cntKey, cost)
  if newv == cost then
    redis.call('PEXPIRE', cntKey, (offset + (bucket+1)*dur - now) + 1000)
  end
  rec = rec .. '|' .. cntKey
end
redis.call('SET', KEYS[2], rec, 'PX', holdTtl)
redis.call('SADD', KEYS[3], KEYS[2])
return {1, 0}
`)

// captureScript settles an admitted request that SUCCEEDED: free the reservation
// from the `held` gauge (by the recorded estimate) and delete the hold record.
// Windows are NOT touched — the spend happened, so its estimate stays counted
// until the window resets (estimate-based model). The actual balance is deducted
// by the caller against the durable #512 ledger, off this script. Idempotent: a
// missing record (already settled / TTL-swept) is a no-op returning 0.
//
// KEYS: [1]=held [2]=hold:<reqID> [3]=holds index set.
var captureScript = redis.NewScript(`
local rec = redis.call('GET', KEYS[2])
if not rec then return 0 end
local sep = string.find(rec, '|', 1, true)
local cost
if sep then cost = tonumber(string.sub(rec, 1, sep-1)) else cost = tonumber(rec) end
if redis.call('INCRBY', KEYS[1], -cost) < 0 then redis.call('SET', KEYS[1], '0') end
redis.call('DEL', KEYS[2])
redis.call('SREM', KEYS[3], KEYS[2])
return 1
`)

// releaseScript backs out an admitted request that FAILED/was cancelled: free the
// reservation from `held` AND from every window counter the admit recorded (the
// request did not happen), then delete the hold record. Bills nothing. Idempotent.
//
// KEYS: [1]=held [2]=hold:<reqID> [3]=holds index set.
var releaseScript = redis.NewScript(`
local rec = redis.call('GET', KEYS[2])
if not rec then return 0 end
local cost = nil
for tok in string.gmatch(rec, '([^|]+)') do
  if cost == nil then
    cost = tonumber(tok)
    if redis.call('INCRBY', KEYS[1], -cost) < 0 then redis.call('SET', KEYS[1], '0') end
  else
    -- decrement only LIVE buckets: INCRBY on an expired bucket would recreate
    -- it as a permanent negative key (#676)
    if redis.call('EXISTS', tok) == 1 then redis.call('INCRBY', tok, -cost) end
  end
end
redis.call('DEL', KEYS[2])
redis.call('SREM', KEYS[3], KEYS[2])
return 1
`)

// Gate is the Redis-backed admission primitive (#513), shared process-wide over
// the same go-redis client the ratelimit.Limiter uses.
//
// The `held` gauge is incremented on admit and decremented on capture/release. A
// crashed in-flight request whose hold record TTL-expires leaves `held` inflated
// (over-reserved → mild under-admission) — self-healed lazily (#676): when the
// affordability gate would deny, the admit script recomputes held = Σ live hold
// records (via the "<base>:holds" index set) and re-checks, so an abandoned admit
// blocks at most until the next denied admit after its hold TTL.
type Gate struct {
	rdb redis.Cmdable
	now func() time.Time
}

// New builds a Gate over a Redis/Garnet client.
func New(rdb redis.Cmdable) *Gate { return &Gate{rdb: rdb, now: time.Now} }

// SetClock overrides the clock (tests).
func (g *Gate) SetClock(now func() time.Time) { g.now = now }

// Decision is the outcome of Admit.
type Decision struct {
	Allowed        bool
	BlockedBalance bool    // true when affordability (balance/credit line) was the binding gate
	BlockedWindow  *Window // the window that blocked, if any
	// RetryAfter is the time until BlockedWindow's next fixed boundary — the
	// earliest moment the same request could succeed. Zero unless a window blocked.
	RetryAfter time.Duration
}

// AdmitInput is one admission request.
type AdmitInput struct {
	Merchant, Customer, Currency string
	RequestID                    string        // idempotency key (provider/tensorhub request id)
	Invoker                      string        // recorded so capture's durable ledger write carries attribution
	Source                       string        // admit-time source namespace (informational; NOT part of the capture coordinate since or#907)
	Cost                         int64         // estimate, minor units
	AccountBalance               int64         // caller's ledger balance snapshot for (payer,currency), minor units
	CreditLimit                  int64         // arrears credit line (0 = prepaid); affordability floor = -CreditLimit
	HoldTTL                      time.Duration // bounds an abandoned hold's life (default 1h)
	Policy                       Policy
	Request                      Request
}

// HoldRef is the payer coordinates a request's hold was placed under, recovered
// by Resolve so capture/release work from the request id alone. Source is the
// admit-time namespace, kept in the record for diagnostics; since or#907 the
// capture's durable coordinate never reads it (it is volatile — the first
// capture consumes this record).
type HoldRef struct {
	Customer string
	Currency string
	Invoker  string
	Source   string
}

// Admit runs the Redis-local atomic affordability + window check and reserves on
// success in one round-trip. Financial callers must hold the PostgreSQL payer
// money lock across their fresh capacity read and this call; Admitter does so.
func (g *Gate) Admit(ctx context.Context, in AdmitInput) (Decision, error) {
	now := g.now()
	base := payerBase(in.Merchant, in.Customer, in.Currency)
	wins := in.Policy.EffectiveWindows(in.Request)

	holdTTL := in.HoldTTL
	if holdTTL <= 0 {
		holdTTL = time.Hour
	}
	keys := []string{base + ":held", base + ":hold:" + in.RequestID, base + ":holds"}
	argv := make([]any, 0, 6+len(wins)*4)
	argv = append(argv, in.Cost, -in.CreditLimit, in.AccountBalance, holdTTL.Milliseconds(), now.UnixMilli(), len(wins))
	for _, w := range wins {
		prefix := w.identity(base)
		dur := w.durationMillis()
		argv = append(argv, prefix, dur, w.Limit, fixedOffsetMs(prefix, dur))
	}

	raw, err := admitScript.Run(ctx, g.rdb, keys, argv...).Result()
	if err != nil {
		return Decision{}, err
	}
	vals, ok := raw.([]any)
	if !ok || len(vals) < 2 {
		return Decision{}, fmt.Errorf("spendgate: unexpected admit result %T", raw)
	}
	dec := Decision{Allowed: toInt64(vals[0]) == 1}
	switch blocked := toInt64(vals[1]); {
	case blocked == -1:
		dec.BlockedBalance = true
	case blocked >= 1 && int(blocked) <= len(wins):
		rw := wins[blocked-1]
		w := rw.Window
		dec.BlockedWindow = &w
		dec.RetryAfter = untilBoundary(now, fixedOffsetMs(rw.identity(base), rw.durationMillis()), rw.durationMillis())
	}
	if dec.Allowed {
		// Record the request→payer pointer so capture/release resolve coords from
		// the request id alone. REQUIRED (#676): a failed write fails the admit
		// (caller won't render an unsettleable request); the leaked reservation
		// self-heals via the hold TTL + blocked-admit recompute.
		ptr := strings.Join([]string{in.Customer, in.Currency, in.Invoker, in.Source}, reqPtrSep)
		if err := g.rdb.Set(ctx, reqPtrKey(in.Merchant, in.RequestID), ptr, holdTTL).Err(); err != nil {
			return Decision{}, fmt.Errorf("spendgate: record admit pointer: %w", err)
		}
	}
	return dec, nil
}

// Resolve returns the payer coordinates a request's hold was placed under (via the
// admit-time pointer). ok=false when no live pointer exists (never admitted, or
// already settled / TTL-expired).
func (g *Gate) Resolve(ctx context.Context, merchant, requestID string) (HoldRef, bool, error) {
	v, e := g.rdb.Get(ctx, reqPtrKey(merchant, requestID)).Result()
	if e == redis.Nil {
		return HoldRef{}, false, nil
	}
	if e != nil {
		return HoldRef{}, false, e
	}
	parts := strings.SplitN(v, reqPtrSep, 4)
	if len(parts) < 2 {
		return HoldRef{}, false, nil
	}
	ref := HoldRef{Customer: parts[0], Currency: parts[1]}
	if len(parts) > 2 {
		ref.Invoker = parts[2]
	}
	if len(parts) > 3 {
		ref.Source = parts[3]
	}
	return ref, true, nil
}

// CaptureInput settles an admitted request that succeeded. The window keys are
// recovered from the hold record, so only the payer coords + request id are needed.
type CaptureInput struct {
	Merchant, Customer, Currency, RequestID string
}

// Capture frees the request's `held` reservation and drops the hold record.
// Windows keep the estimate (estimate-based model); the actual balance deduction +
// durable #512 ledger write are the caller's job, off the hot path. Idempotent.
func (g *Gate) Capture(ctx context.Context, in CaptureInput) error {
	base := payerBase(in.Merchant, in.Customer, in.Currency)
	keys := []string{base + ":held", base + ":hold:" + in.RequestID, base + ":holds"}
	if err := captureScript.Run(ctx, g.rdb, keys).Err(); err != nil {
		return err
	}
	g.rdb.Del(ctx, reqPtrKey(in.Merchant, in.RequestID))
	return nil
}

// ReleaseInput backs out an admitted-but-uncharged request (failure path).
type ReleaseInput struct {
	Merchant, Customer, Currency, RequestID string
}

// Release frees the reservation from `held` and from every window counter the
// admit recorded (the request did not happen), without billing. Idempotent.
func (g *Gate) Release(ctx context.Context, in ReleaseInput) error {
	base := payerBase(in.Merchant, in.Customer, in.Currency)
	keys := []string{base + ":held", base + ":hold:" + in.RequestID, base + ":holds"}
	if err := releaseScript.Run(ctx, g.rdb, keys).Err(); err != nil {
		return err
	}
	g.rdb.Del(ctx, reqPtrKey(in.Merchant, in.RequestID))
	return nil
}

// HeldAmount returns the current shared in-flight reservation total for a
// payer+currency (introspection / start-capacity display).
func (g *Gate) HeldAmount(ctx context.Context, merchant, customer, currency string) (int64, error) {
	n, err := g.rdb.Get(ctx, payerBase(merchant, customer, currency)+":held").Int64()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return n, nil
}

// fixedOffsetMs is the deterministic per-window phase (in [0, durMs)) for a FIXED
// window, derived from its customer-scoped key. Two payers' same-shaped windows
// hash to different offsets, so their reset boundaries (offset + k*dur) are
// staggered — spreading demand instead of every payer resetting at the same
// instant — WITHOUT storing a per-user anchor (recomputable, flush-safe).
func fixedOffsetMs(prefix string, durMs int64) int64 {
	if durMs <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(prefix))
	// durMs > 0 is guaranteed above, so the modulo result is in [0, durMs) and
	// always fits a non-negative int64; safecast keeps the conversion checked.
	off, _ := safecast.Convert[int64](h.Sum64() % uint64(durMs))
	return off
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

// untilBoundary is the time from now to the next boundary of a fixed window with
// phase offsetMs and period durMs (boundaries at offset + k*dur).
func untilBoundary(now time.Time, offsetMs, durMs int64) time.Duration {
	if durMs <= 0 {
		return 0
	}
	nowMs := now.UnixMilli()
	bucket := (nowMs - offsetMs) / durMs
	return time.Duration(offsetMs+(bucket+1)*durMs-nowMs) * time.Millisecond
}
