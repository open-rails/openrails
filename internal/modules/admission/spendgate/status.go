package spendgate

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// WindowStatus is one effective window's current state for introspection (no
// mutation). Used is the current bucket's running total (estimate-based:
// reserved + captured combined); Remaining = Limit − Used; ResetAt is when the
// window's bucket next rolls.
type WindowStatus struct {
	Window
	ScopeID   string
	Used      int64
	Remaining int64
	ResetAt   time.Time
}

// WindowStatus reads every effective window's current counter for (payer,
// currency) WITHOUT reserving — powers a host's budget dashboard. Off the hot
// path: a couple of reads per window.
func (g *Gate) WindowStatus(ctx context.Context, merchant, customer, currency string, policy Policy, req Request) ([]WindowStatus, error) {
	now := g.now()
	base := payerBase(merchant, customer, currency)
	wins := policy.EffectiveWindows(req)
	out := make([]WindowStatus, 0, len(wins))
	for _, w := range wins {
		prefix := w.identity(base)
		dur := w.Duration
		var used int64
		var resetAt time.Time
		switch w.Cadence {
		case CadenceFixed:
			durMs := dur.Milliseconds()
			offset := fixedOffsetMs(prefix, durMs)
			nowMs := now.UnixMilli()
			bucket := (nowMs - offset) / durMs
			cnt, _, err := getInt(ctx, g.rdb, prefix+":"+strconv.FormatInt(bucket, 10))
			if err != nil {
				return nil, err
			}
			used = cnt
			resetAt = time.UnixMilli(offset + (bucket+1)*durMs)
		default: // session
			cnt, ok, err := getInt(ctx, g.rdb, prefix)
			if err != nil {
				return nil, err
			}
			used = cnt
			if ok {
				ttl, err := g.rdb.PTTL(ctx, prefix).Result()
				if err != nil {
					return nil, err
				}
				if ttl > 0 {
					resetAt = now.Add(ttl)
				} else {
					resetAt = now.Add(dur)
				}
			} else {
				resetAt = now.Add(dur)
			}
		}
		out = append(out, WindowStatus{
			Window:    w.Window,
			ScopeID:   w.scopeID,
			Used:      used,
			Remaining: w.Limit - used,
			ResetAt:   resetAt,
		})
	}
	return out, nil
}

// getInt reads an integer key; ok=false when the key is absent.
func getInt(ctx context.Context, rdb redis.Cmdable, key string) (int64, bool, error) {
	n, err := rdb.Get(ctx, key).Int64()
	if err == redis.Nil {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}
