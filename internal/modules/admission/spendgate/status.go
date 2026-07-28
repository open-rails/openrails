package spendgate

import (
	"context"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// WindowUsage is one effective window's current bucket total (estimate-based:
// reserved + captured combined). Read-only introspection, off the hot path.
type WindowUsage struct {
	Window
	Used int64
}

// WindowUsage reads every effective window's current counter for (payer,
// currency) WITHOUT reserving.
func (g *Gate) WindowUsage(ctx context.Context, merchant, customer, currency string, policy Policy, req Request) ([]WindowUsage, error) {
	now := g.now()
	base := payerBase(merchant, customer, currency)
	wins := policy.EffectiveWindows(req)
	out := make([]WindowUsage, 0, len(wins))
	for _, w := range wins {
		prefix := w.identity(base)
		durMs := w.Duration.Milliseconds()
		bucket := (now.UnixMilli() - fixedOffsetMs(prefix, durMs)) / durMs
		used, err := g.rdb.Get(ctx, prefix+":"+strconv.FormatInt(bucket, 10)).Int64()
		if err == redis.Nil {
			used = 0
		} else if err != nil {
			return nil, err
		}
		out = append(out, WindowUsage{Window: w.Window, Used: used})
	}
	return out, nil
}
