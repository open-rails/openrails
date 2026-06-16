// Package ratelimit is OpenRails' own fixed-window + (later) concurrency rate
// limiter for customer API throughput (issue #298). It is the THROUGHPUT axis of
// admission control: RPM/RPD/TPM/TPD per endpoint(=model) over arbitrary unit
// types (request/token/image/gpu-second). The MONEY axis stays on the credit
// ledger; this package never touches Postgres.
//
// Built natively (the user's cozy-creator/ratelimiter is a reference only, not
// imported). Counters live in Redis/Garnet. The check+increment across all
// windows is one atomic server-side op via a Lua script (portable across Redis
// and Garnet; the interpreter overhead is microseconds, dominated by the network
// RTT — a Garnet C# custom-transaction is a later, Garnet-committed optimization).
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limit is one fixed-window throughput limit: at most Max units of Unit per Window.
type Limit struct {
	// Unit is any arbitrary unit string: "request" (default; RPM/RPD), "token"
	// (TPM/TPD), "image" (IPM), "video" (VPM), "gpu_second", ... — "per minute"
	// vs "per day" is just Window. The whichever-window-first deny means RPM+TPM
	// (or any mix, incl. images/min + videos/min) compose for free (#472).
	Unit   string        // "request" (default), "token", "image", "video", "gpu_second", ...
	Window time.Duration // e.g. time.Minute (RPM/TPM/IPM/VPM), 24h (RPD/TPD)
	Max    int64
}

// Policy is the set of windows enforced for one (endpoint/model) at a tier. A
// request is denied if it would breach ANY window (whichever is hit first).
type Policy struct {
	Windows []Limit
}

// WindowInfo is the post-check state of one window, for x-ratelimit-* headers.
type WindowInfo struct {
	Unit       string        `json:"unit"`
	Limit      int64         `json:"limit"`
	Remaining  int64         `json:"remaining"`
	ResetAfter time.Duration `json:"reset_after"`
}

// Decision is the outcome of an admission check.
type Decision struct {
	Allowed     bool         `json:"allowed"`
	BlockedUnit string       `json:"blocked_unit,omitempty"` // the window that denied it
	Windows     []WindowInfo `json:"windows"`
}

// Limiter evaluates throughput policies against a Redis/Garnet store.
type Limiter struct {
	rdb redis.Cmdable
	now func() time.Time
}

// NewLimiter builds a Limiter over a Redis/Garnet client.
func NewLimiter(rdb redis.Cmdable) *Limiter {
	return &Limiter{rdb: rdb, now: time.Now}
}

// SetClock overrides the clock (tests).
func (l *Limiter) SetClock(now func() time.Time) { l.now = now }

// checkScript atomically: (1) reads every window's current count, (2) if EVERY
// window can fit its requested amount, increments them all (all-or-nothing, so a
// blocked request never partially consumes counters), else increments nothing.
// Returns [blockedIndex, cur1, cur2, ...]: blockedIndex==0 means allowed and the
// currents are post-increment; >0 means blocked at that window (1-based) and the
// currents are the un-incremented current values.
var checkScript = redis.NewScript(`
local n = tonumber(ARGV[1])
local currents = {}
local blocked = 0
for i=1,n do
  local max = tonumber(ARGV[(i-1)*3 + 2])
  local amt = tonumber(ARGV[(i-1)*3 + 3])
  local cur = tonumber(redis.call('GET', KEYS[i]) or '0')
  currents[i] = cur
  if amt > 0 and cur + amt > max then blocked = i end
end
if blocked == 0 then
  for i=1,n do
    local amt = tonumber(ARGV[(i-1)*3 + 3])
    local ttl = tonumber(ARGV[(i-1)*3 + 4])
    if amt > 0 then
      local newv = redis.call('INCRBY', KEYS[i], amt)
      currents[i] = newv
      if newv == amt then redis.call('PEXPIRE', KEYS[i], ttl) end
    end
  end
end
local res = {blocked}
for i=1,n do res[#res+1] = currents[i] end
return res
`)

// Check evaluates the policy for one logical bucket (base, e.g.
// "<merchant>:<owner>:<model>") given the unit amounts this request consumes
// (e.g. {"request":1,"token":150}). It is atomic and all-or-nothing.
func (l *Limiter) Check(ctx context.Context, base string, p Policy, amounts map[string]int64) (Decision, error) {
	if len(p.Windows) == 0 {
		return Decision{Allowed: true}, nil
	}
	now := l.now().UTC()
	keys := make([]string, len(p.Windows))
	argv := make([]interface{}, 0, 1+len(p.Windows)*3)
	argv = append(argv, len(p.Windows))
	for i, w := range p.Windows {
		secs := int64(w.Window / time.Second)
		if secs <= 0 {
			secs = 1
		}
		bucket := now.Unix() / secs
		keys[i] = fmt.Sprintf("rl:%s:%s:%d:%d", base, w.Unit, secs, bucket)
		amt := amounts[w.Unit]
		ttlMs := secs*1000 + 1000 // window + 1s slack
		argv = append(argv, w.Max, amt, ttlMs)
	}

	raw, err := checkScript.Run(ctx, l.rdb, keys, argv...).Result()
	if err != nil {
		return Decision{}, err
	}
	vals, ok := raw.([]interface{})
	if !ok || len(vals) < 1+len(p.Windows) {
		return Decision{}, fmt.Errorf("ratelimit: unexpected script result %T", raw)
	}
	blocked := toInt64(vals[0])

	dec := Decision{Allowed: blocked == 0, Windows: make([]WindowInfo, len(p.Windows))}
	for i, w := range p.Windows {
		secs := int64(w.Window / time.Second)
		if secs <= 0 {
			secs = 1
		}
		cur := toInt64(vals[1+i])
		remaining := w.Max - cur
		if remaining < 0 {
			remaining = 0
		}
		resetAfter := time.Duration((secs - (now.Unix() % secs))) * time.Second
		dec.Windows[i] = WindowInfo{Unit: w.Unit, Limit: w.Max, Remaining: remaining, ResetAfter: resetAfter}
	}
	if blocked > 0 && int(blocked) <= len(p.Windows) {
		dec.BlockedUnit = p.Windows[blocked-1].Unit
	}
	return dec, nil
}

func toInt64(v interface{}) int64 {
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
