package spendgate

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// WindowUsage is one effective window's live state, read WITHOUT reserving.
//
// Used is the current bucket total. Windows are ESTIMATE-BASED, so Used already
// includes the estimates of admits still in flight; Reserved names that in-flight
// part — what a release would hand back — derived from the SAME hold records the
// admit script writes. There is no per-window reserved counter and adding one
// would be a second home for a number Redis already holds.
//
// ResetsAt is the window's real next boundary (offset + (bucket+1)*duration on
// the deterministic per-payer phase), never now+duration.
type WindowUsage struct {
	Window
	Used     int64
	Reserved int64
	ResetsAt time.Time
}

// WindowUsage reads every effective window's live state for (payer, currency)
// WITHOUT reserving. Off the hot path.
func (g *Gate) WindowUsage(ctx context.Context, merchant, customer, currency string, policy Policy, req Request) ([]WindowUsage, error) {
	now := g.now()
	base := payerBase(merchant, customer, currency)
	wins := policy.EffectiveWindows(req)
	if len(wins) == 0 {
		return nil, nil
	}
	reserved, err := g.reservedByWindowKey(ctx, base)
	if err != nil {
		return nil, err
	}
	out := make([]WindowUsage, 0, len(wins))
	for _, w := range wins {
		prefix := w.identity(base)
		durMs := w.durationMillis()
		offset := fixedOffsetMs(prefix, durMs)
		bucket := (now.UnixMilli() - offset) / durMs
		counter := prefix + ":" + strconv.FormatInt(bucket, 10)
		used, gerr := g.rdb.Get(ctx, counter).Int64()
		if gerr == redis.Nil {
			used = 0
		} else if gerr != nil {
			return nil, gerr
		}
		out = append(out, WindowUsage{
			Window:   w.Window,
			Used:     used,
			Reserved: reserved[counter],
			ResetsAt: now.Add(untilBoundary(now, offset, durMs)),
		})
	}
	return out, nil
}

// reservedByWindowKey sums the LIVE holds against each window counter key they
// incremented. The admit script stores "<cost>|<windowKey>|…" per request and
// indexes the records in "<base>:holds"; capture and release delete them, so a
// record that is still readable is still reserved. A member whose record has
// TTL-expired (abandoned admit, #676) is not a live reservation and is skipped —
// the same lazy view the admit script's recompute takes.
func (g *Gate) reservedByWindowKey(ctx context.Context, base string) (map[string]int64, error) {
	members, err := g.rdb.SMembers(ctx, base+":holds").Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(members))
	for _, member := range members {
		record, gerr := g.rdb.Get(ctx, member).Result()
		if gerr == redis.Nil {
			continue
		}
		if gerr != nil {
			return nil, gerr
		}
		fields := strings.Split(record, "|")
		if len(fields) < 2 {
			continue
		}
		cost, cerr := strconv.ParseInt(fields[0], 10, 64)
		if cerr != nil {
			continue
		}
		for _, counter := range fields[1:] {
			out[counter] += cost
		}
	}
	return out, nil
}
