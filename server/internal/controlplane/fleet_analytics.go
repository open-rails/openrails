package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
)

// Fleet analytics: cross-merchant operator aggregates over the engine's truth
// tables, never merchant rows. The caller gates and audits each request.

// FleetMerchantFunnel counts merchants by lifecycle stage: provisioned (total),
// armed (a live PSP declared), first-revenue (any completed payment ever), and
// active (a completed payment inside the window).
type FleetMerchantFunnel struct {
	Total         int64
	Armed         int64
	FirstRevenue  int64
	ActiveRevenue int64
}

// FleetCurrencyRevenue is the window's settled volume in one currency, in the
// currency's native units (the ledger convention, docs/money-wire.md).
type FleetCurrencyRevenue struct {
	Currency      string
	Payments      int64
	SettledAmount int64
}

// FleetRailHealth is one rail's window outcome split across the fleet.
// Chargebacks counts reversal_kind='chargeback' mirror rows recorded in the
// window: the dispute signal VAMP-style monitoring watches.
type FleetRailHealth struct {
	Rail        string
	Succeeded   int64
	Failed      int64
	Chargebacks int64
}

// FleetMRR is the monthly-normalized recurring run-rate in one currency, in
// native units: each active auto-renew subscription's price scaled by 720h/period.
type FleetMRR struct {
	Currency      string
	Subscriptions int64
	MonthlyAmount int64
}

// FleetAnalytics is one consistent operator snapshot of the hosted fleet.
type FleetAnalytics struct {
	WindowDays int
	Merchants  FleetMerchantFunnel
	Revenue    []FleetCurrencyRevenue
	Rails      []FleetRailHealth
	MRR        []FleetMRR
}

// FleetAnalytics aggregates the fleet snapshot. exclude removes one merchant
// from every aggregate (the platform merchant itself, so its self-billing book
// never counts as fleet processing volume); zero means exclude nothing.
// windowDays outside 1..365 is refused (billing.ErrInvalid).
func (c *ControlPlane) FleetAnalytics(ctx context.Context, exclude billing.MerchantID, windowDays int) (*FleetAnalytics, error) {
	if windowDays < 1 || windowDays > 365 {
		return nil, invalidRange("windowDays", 1, 365)
	}
	if c == nil || c.pool == nil {
		return nil, errors.New("controlplane: pgx pool unavailable for fleet analytics")
	}
	since := time.Now().UTC().AddDate(0, 0, -windowDays)
	var excludeArg *uuid.UUID
	if !exclude.IsZero() {
		id := exclude.UUID()
		excludeArg = &id
	}

	// Every aggregate below reads merchant-owned tables (payments,
	// subscriptions, prices, psps) and returns counts and sums only.
	out := &FleetAnalytics{WindowDays: windowDays}
	q := gen.New(c.pool)
	funnel, err := q.FleetMerchantFunnel(ctx, gen.FleetMerchantFunnelParams{ExcludeMerchantID: excludeArg, Since: since})
	if err != nil {
		return nil, fmt.Errorf("fleet analytics: merchant funnel: %w", err)
	}
	out.Merchants.Total, out.Merchants.Armed = funnel.Total, funnel.Armed
	out.Merchants.FirstRevenue, out.Merchants.ActiveRevenue = funnel.FirstRevenue, funnel.ActiveRevenue

	// Sale rows only: refund/chargeback mirror rows share status='completed'
	// (with negative amounts + reversal_kind set) and must not count as sales.
	revenue, err := q.FleetRevenueByCurrency(ctx, gen.FleetRevenueByCurrencyParams{ExcludeMerchantID: excludeArg, Since: since})
	if err != nil {
		return nil, fmt.Errorf("fleet analytics: revenue: %w", err)
	}
	for _, r := range revenue {
		out.Revenue = append(out.Revenue, FleetCurrencyRevenue{Currency: r.Currency, Payments: r.Payments, SettledAmount: r.SettledAmount})
	}

	rails, err := q.FleetRailHealth(ctx, gen.FleetRailHealthParams{ExcludeMerchantID: excludeArg, Since: since})
	if err != nil {
		return nil, fmt.Errorf("fleet analytics: rail health: %w", err)
	}
	for _, r := range rails {
		out.Rails = append(out.Rails, FleetRailHealth{Rail: r.Rail, Succeeded: r.Succeeded, Failed: r.Failed, Chargebacks: r.Chargebacks})
	}

	mrr, err := q.FleetMRRByCurrency(ctx, excludeArg)
	if err != nil {
		return nil, fmt.Errorf("fleet analytics: mrr: %w", err)
	}
	for _, r := range mrr {
		out.MRR = append(out.MRR, FleetMRR{Currency: r.Currency, Subscriptions: r.Subscriptions, MonthlyAmount: r.MonthlyAmount})
	}
	return out, nil
}
