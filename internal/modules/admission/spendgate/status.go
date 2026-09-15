package spendgate

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

type WindowUsage struct {
	Window
	Used     int64
	Reserved int64
	ResetsAt time.Time
}

// Reads and admission use the same durable facts, including expired work risk.
func (g *Gate) WindowUsage(ctx context.Context, payer uuid.UUID, currency string, policy Policy, req Request) ([]WindowUsage, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	now := g.Now()
	windows := policy.EffectiveWindows(req)
	out := make([]WindowUsage, 0, len(windows))
	for _, w := range windows {
		key, start, end, err := windowPeriod(mid.UUID(), payer, currency, w, now)
		if err != nil {
			return nil, err
		}
		usage, err := g.db.Gen(ctx).AdmissionWindowUsage(ctx, gen.AdmissionWindowUsageParams{
			MerchantID: mid.UUID(), PayerID: payer, Currency: currency, WindowKey: key, WindowStart: start, WindowEnd: end, AsOf: now,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, WindowUsage{Window: w.Window, Used: usage.Used, Reserved: usage.Reserved, ResetsAt: end})
	}
	return out, nil
}
