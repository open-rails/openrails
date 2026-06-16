package abuse

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/ratelimit"
)

// WastedSpendGuard tracks $-VALUED wasted/failed spend in rolling fixed windows
// (#488), generalizing the event-COUNT velocity windows so a pricey failure
// weighs more than a cheap one. Every platform has work that FAILS and costs it
// money — a hold released without capture, a host-reported out-of-band cost — and
// the prepaid balance can't bound it (failures are refunded). The host REPORTS
// the wasted $; direct-payer reports use payer grace then ledger charging, while
// delegated invokers are cut off at admit when their flat counter is over.
//
// Two independent subjects, each multi-window and currency-scoped:
//   - PAYER ("wp:<merchant>:<payer>:<currency>") — direct-payer grace graduated by tier.
//   - INVOKER ("wa:<merchant>:<payer>:<invoker>:<currency>") — a flat config default (invokers
//     aren't trusted: an account can mint unlimited invokers, so the per-invoker
//     budget is a fixed backstop, not graduated). Payer is part of the key because
//     delegated-user IDs are only guaranteed unique inside the payer/platform.
//
// Built on the proven Redis fixed-window limiter (same infra as throughput +
// velocity); no new store. Everything is generic money amount accounting — no host concepts.
type WastedSpendGuard struct {
	lim *ratelimit.Limiter
}

func NewWastedSpendGuard(lim *ratelimit.Limiter) *WastedSpendGuard {
	return &WastedSpendGuard{lim: lim}
}

// Enabled reports whether the guard has a limiter wired (nil = safe no-op so the
// admit/report path never breaks when Redis isn't configured).
func (g *WastedSpendGuard) Enabled() bool { return g != nil && g.lim != nil }

// WastedWindow is one $-valued wasted-spend budget window: at most Limit of
// reported wasted $ per Window. Mirrors a money-budget window but for FAILED spend.
type WastedWindow struct {
	Key      string
	Window   time.Duration
	Limit    int64
	Currency string
}

// WindowUsage is the running wasted-$ total + budget for one window (introspection
// for /abuse-usage). OverBudget when Used >= Limit.
type WindowUsage struct {
	Key        string `json:"key"`
	Currency   string `json:"currency"`
	Window     string `json:"window"`
	Used       int64  `json:"used"`
	Limit      int64  `json:"limit"`
	OverBudget bool   `json:"over_budget"`
}

func payerBase(merchant, payer, currency string) string {
	return fmt.Sprintf("wp:%s:%s:%s", merchant, payer, currency)
}
func invokerBase(merchant, payer, invoker, currency string) string {
	return fmt.Sprintf("wa:%s:%s:%s:%s", merchant, payer, invoker, currency)
}

// ClaimReport records short-lived idempotency for a host-reported wasted-spend
// event. The TTL should be the largest active wasted-spend window so retries do
// not double-add hot counters while the report can affect enforcement.
func (g *WastedSpendGuard) ClaimReport(ctx context.Context, merchant, payer, currency, source, sourceID string, ttl time.Duration) (bool, error) {
	if !g.Enabled() {
		return false, nil
	}
	source = strings.TrimSpace(source)
	sourceID = strings.TrimSpace(sourceID)
	if source == "" || sourceID == "" {
		return false, fmt.Errorf("wasted-spend source and source_id required")
	}
	return g.lim.ClaimOnce(ctx, fmt.Sprintf("wasted:%s:%s:%s:%s:%s", merchant, payer, currency, source, sourceID), ttl)
}

// Record adds an amount of wasted spend to BOTH the payer's and the invoker's windows
// (whichever window lists are supplied). The unit is the window Key, so multiple
// windows of one subject are distinct Redis counters. A no-op for amount <= 0
// (cheap rejects are not reported).
func (g *WastedSpendGuard) Record(ctx context.Context, merchant, payer, invoker, currency string, amount int64, payerWindows, invokerWindows []WastedWindow) error {
	if !g.Enabled() || amount <= 0 {
		return nil
	}
	if payer != "" {
		base := payerBase(merchant, payer, currency)
		for _, w := range payerWindows {
			if _, err := g.lim.AddWindowValue(ctx, base, w.Key, w.Window, amount); err != nil {
				return err
			}
		}
	}
	if invoker != "" {
		base := invokerBase(merchant, payer, invoker, currency)
		for _, w := range invokerWindows {
			if _, err := g.lim.AddWindowValue(ctx, base, w.Key, w.Window, amount); err != nil {
				return err
			}
		}
	}
	return nil
}

// RecordPayerGrace records direct-payer wasted spend against payer grace windows
// and returns the amount that exceeds the strictest remaining grace window. No
// windows means no configured grace policy and no chargeable overage.
func (g *WastedSpendGuard) RecordPayerGrace(ctx context.Context, merchant, payer, currency string, amount int64, windows []WastedWindow) (int64, error) {
	if !g.Enabled() || amount <= 0 || payer == "" {
		return 0, nil
	}
	base := payerBase(merchant, payer, currency)
	hasWindow := false
	freeRemaining := int64(0)
	for _, w := range windows {
		if w.Limit <= 0 || w.Window <= 0 {
			continue
		}
		used, err := g.lim.WindowValue(ctx, base, w.Key, w.Window)
		if err != nil {
			return 0, err
		}
		remaining := w.Limit - used
		if remaining < 0 {
			remaining = 0
		}
		if !hasWindow || remaining < freeRemaining {
			freeRemaining = remaining
			hasWindow = true
		}
	}
	for _, w := range windows {
		if w.Window <= 0 {
			continue
		}
		if _, err := g.lim.AddWindowValue(ctx, base, w.Key, w.Window, amount); err != nil {
			return 0, err
		}
	}
	if !hasWindow || amount <= freeRemaining {
		return 0, nil
	}
	return amount - freeRemaining, nil
}

// RecordInvokerCutoff records delegated-invoker wasted spend against the flat
// per-invoker cutoff windows.
func (g *WastedSpendGuard) RecordInvokerCutoff(ctx context.Context, merchant, payer, invoker, currency string, amount int64, windows []WastedWindow) error {
	if !g.Enabled() || amount <= 0 || invoker == "" {
		return nil
	}
	base := invokerBase(merchant, payer, invoker, currency)
	for _, w := range windows {
		if w.Window <= 0 {
			continue
		}
		if _, err := g.lim.AddWindowValue(ctx, base, w.Key, w.Window, amount); err != nil {
			return err
		}
	}
	return nil
}

// InvokerOverBudget reports whether the invoker's running wasted-$ total is at/over
// ANY of its budget windows (the admit gate for failure_rate_limited).
func (g *WastedSpendGuard) InvokerOverBudget(ctx context.Context, merchant, payer, invoker, currency string, windows []WastedWindow) (bool, string, error) {
	return g.overBudget(ctx, invokerBase(merchant, payer, invoker, currency), windows)
}

func (g *WastedSpendGuard) overBudget(ctx context.Context, base string, windows []WastedWindow) (bool, string, error) {
	if !g.Enabled() {
		return false, "", nil
	}
	for _, w := range windows {
		if w.Limit <= 0 {
			continue
		}
		used, err := g.lim.WindowValue(ctx, base, w.Key, w.Window)
		if err != nil {
			return false, "", err
		}
		if used >= w.Limit {
			return true, w.Key, nil
		}
	}
	return false, "", nil
}

// Usage returns the running wasted-$ totals for a subject's windows (introspection
// for /abuse-usage). subject "payer" or "invoker" selects the keyspace.
func (g *WastedSpendGuard) Usage(ctx context.Context, base, currency string, windows []WastedWindow) ([]WindowUsage, error) {
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
			Key: w.Key, Currency: currency, Window: w.Window.String(), Used: used,
			Limit: w.Limit, OverBudget: w.Limit > 0 && used >= w.Limit,
		})
	}
	return out, nil
}

// PayerBase / InvokerBase expose the keyspace prefixes so a caller (the service
// facade /abuse-usage) can read Usage for either subject.
func (g *WastedSpendGuard) PayerBase(merchant, payer, currency string) string {
	return payerBase(merchant, payer, currency)
}
func (g *WastedSpendGuard) InvokerBase(merchant, payer, invoker, currency string) string {
	return invokerBase(merchant, payer, invoker, currency)
}
