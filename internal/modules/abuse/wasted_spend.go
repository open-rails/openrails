package abuse

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/modules/ratelimit"
)

// WastedSpendGuard tracks $-VALUED wasted/failed spend in rolling fixed windows
// (#488), generalizing the event-COUNT velocity windows so a pricey failure
// weighs more than a cheap one. Every platform has work that FAILS and costs it
// money — a hold released without capture, a host-reported out-of-band cost — and
// the prepaid balance can't bound it (failures are refunded). The host REPORTS
// the wasted $ (Record); admit enforces a budget over the running total (Check).
//
// Two independent subjects, each multi-window:
//   - PAYER ("wp:<tenant>:<payer>") — budget graduated by the tier policy
//     (a per-tier bad_spend $ budget).
//   - ACTOR ("wa:<tenant>:<actor>") — a flat config default (invokers aren't
//     trusted: an account can mint unlimited invokers, so the per-actor budget is
//     a fixed backstop, not graduated).
//
// Built on the proven Redis fixed-window limiter (same infra as throughput +
// velocity); no new store. Everything is $-micros + generic — no host concepts.
type WastedSpendGuard struct {
	lim *ratelimit.Limiter
}

func NewWastedSpendGuard(lim *ratelimit.Limiter) *WastedSpendGuard {
	return &WastedSpendGuard{lim: lim}
}

// Enabled reports whether the guard has a limiter wired (nil = safe no-op so the
// admit/report path never breaks when Redis isn't configured).
func (g *WastedSpendGuard) Enabled() bool { return g != nil && g.lim != nil }

// WastedWindow is one $-valued wasted-spend budget window: at most LimitMicros of
// reported wasted $ per Window. Mirrors a money-budget window but for FAILED spend.
type WastedWindow struct {
	Key         string
	Window      time.Duration
	LimitMicros int64
}

// WindowUsage is the running wasted-$ total + budget for one window (introspection
// for /abuse-usage). OverBudget when UsedMicros >= LimitMicros.
type WindowUsage struct {
	Key         string `json:"key"`
	Window      string `json:"window"`
	UsedMicros  int64  `json:"used_micros"`
	LimitMicros int64  `json:"limit_micros"`
	OverBudget  bool   `json:"over_budget"`
}

func payerBase(tenant, payer string) string { return fmt.Sprintf("wp:%s:%s", tenant, payer) }
func actorBase(tenant, actor string) string { return fmt.Sprintf("wa:%s:%s", tenant, actor) }

// Record adds micros of wasted spend to BOTH the payer's and the actor's windows
// (whichever window lists are supplied). The unit is the window Key, so multiple
// windows of one subject are distinct Redis counters. Best-effort idempotency is
// the caller's job (it keys the report); this just accumulates. A no-op for
// micros <= 0 (cheap rejects ≈ $0 aren't reported).
func (g *WastedSpendGuard) Record(ctx context.Context, tenant, payer, actor string, micros int64, payerWindows, actorWindows []WastedWindow) error {
	if !g.Enabled() || micros <= 0 {
		return nil
	}
	if payer != "" {
		base := payerBase(tenant, payer)
		for _, w := range payerWindows {
			if _, err := g.lim.AddWindowValue(ctx, base, w.Key, w.Window, micros); err != nil {
				return err
			}
		}
	}
	if actor != "" {
		base := actorBase(tenant, actor)
		for _, w := range actorWindows {
			if _, err := g.lim.AddWindowValue(ctx, base, w.Key, w.Window, micros); err != nil {
				return err
			}
		}
	}
	return nil
}

// PayerOverBudget reports whether the payer's running wasted-$ total is at/over
// ANY of its budget windows (the admit gate for abuse_rate_limited). Returns the
// breached window key.
func (g *WastedSpendGuard) PayerOverBudget(ctx context.Context, tenant, payer string, windows []WastedWindow) (bool, string, error) {
	return g.overBudget(ctx, payerBase(tenant, payer), windows)
}

// ActorOverBudget reports whether the actor's running wasted-$ total is at/over
// ANY of its budget windows (the admit gate for failure_rate_limited).
func (g *WastedSpendGuard) ActorOverBudget(ctx context.Context, tenant, actor string, windows []WastedWindow) (bool, string, error) {
	return g.overBudget(ctx, actorBase(tenant, actor), windows)
}

func (g *WastedSpendGuard) overBudget(ctx context.Context, base string, windows []WastedWindow) (bool, string, error) {
	if !g.Enabled() {
		return false, "", nil
	}
	for _, w := range windows {
		if w.LimitMicros <= 0 {
			continue
		}
		used, err := g.lim.WindowValue(ctx, base, w.Key, w.Window)
		if err != nil {
			return false, "", err
		}
		if used >= w.LimitMicros {
			return true, w.Key, nil
		}
	}
	return false, "", nil
}

// Usage returns the running wasted-$ totals for a subject's windows (introspection
// for /abuse-usage). subject "payer" or "actor" selects the keyspace.
func (g *WastedSpendGuard) Usage(ctx context.Context, base string, windows []WastedWindow) ([]WindowUsage, error) {
	out := make([]WindowUsage, 0, len(windows))
	if !g.Enabled() {
		return out, nil
	}
	for _, w := range windows {
		used, err := g.lim.WindowValue(ctx, base, w.Key, w.Window)
		if err != nil {
			return nil, err
		}
		out = append(out, WindowUsage{
			Key: w.Key, Window: w.Window.String(), UsedMicros: used,
			LimitMicros: w.LimitMicros, OverBudget: w.LimitMicros > 0 && used >= w.LimitMicros,
		})
	}
	return out, nil
}

// PayerBase / ActorBase expose the keyspace prefixes so a caller (the service
// facade /abuse-usage) can read Usage for either subject.
func (g *WastedSpendGuard) PayerBase(tenant, payer string) string { return payerBase(tenant, payer) }
func (g *WastedSpendGuard) ActorBase(tenant, actor string) string { return actorBase(tenant, actor) }
