package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/open-rails/openrails/pkg/merchant"
)

// Fleet analytics (openrails-saas #28): cross-merchant operator aggregates over
// the engine's truth tables. Like SearchMerchants (#226) this is a sensitive
// cross-merchant read — no per-merchant RLS scope could compute a fleet view —
// and the CALLER is responsible for gating (platform superadmin) and auditing
// each request. It reaches across merchants the ONE sanctioned way: migration
// 0022's SECURITY DEFINER aggregates. The control-plane pool is not privileged;
// it is the same role and the same DSN as the app's (or#861).

// FleetMerchantFunnel counts merchants by lifecycle stage: provisioned (total),
// armed (a live PSP declared), first-revenue (any completed payment ever), and
// active (a completed payment inside the window).
type FleetMerchantFunnel struct {
	Total         int64
	Armed         int64
	FirstRevenue  int64
	ActiveRevenue int64
}

// FleetCurrencyRevenue is the window's settled volume in one currency, in
// MICROS (millionths of a major unit — the ledger convention).
type FleetCurrencyRevenue struct {
	Currency      string
	Payments      int64
	SettledAmount int64
}

// FleetRailHealth is one rail's window outcome split across the fleet.
// Chargebacks counts reversal_kind='chargeback' mirror rows recorded in the
// window (#733) — the dispute signal VAMP-style monitoring watches.
type FleetRailHealth struct {
	Rail        string
	Succeeded   int64
	Failed      int64
	Chargebacks int64
}

// FleetMRR is the monthly-normalized recurring run-rate in one currency, in
// MICROS: each active auto-renew subscription's price scaled by 720h/period.
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
// windowDays outside 1..365 falls back to 30.
func (c *ControlPlane) FleetAnalytics(ctx context.Context, exclude merchant.ID, windowDays int) (*FleetAnalytics, error) {
	if c == nil || c.pool == nil {
		return nil, errors.New("controlplane: pgx pool unavailable for fleet analytics")
	}
	if windowDays < 1 || windowDays > 365 {
		windowDays = 30
	}
	since := time.Now().UTC().AddDate(0, 0, -windowDays)
	var excludeArg any
	if !exclude.IsZero() {
		excludeArg = exclude.UUID()
	}

	// or#861: every aggregate below reads RLS-bearing tables (payments,
	// subscriptions, prices, psps). Read on the base pool they returned ZERO
	// ROWS AND NO ERROR under the production openrails_app role, so this
	// dashboard reported all zeros — it only ever "worked" where the connected
	// role bypassed RLS. A fleet dashboard IS a genuinely cross-merchant read,
	// so it goes through migration 0022's SECURITY DEFINER aggregates: they
	// RAISE when their definer cannot bypass RLS (no silent zeros ever again)
	// and they return AGGREGATES ONLY — counts and sums grouped by
	// currency/rail — never a merchant-owned row.
	out := &FleetAnalytics{WindowDays: windowDays}
	if err := c.pool.QueryRow(ctx,
		`SELECT total, armed, first_revenue, active_revenue
		   FROM openrails.fleet_merchant_funnel($1::uuid, $2::timestamptz)`,
		excludeArg, since).Scan(
		&out.Merchants.Total, &out.Merchants.Armed,
		&out.Merchants.FirstRevenue, &out.Merchants.ActiveRevenue,
	); err != nil {
		return nil, fmt.Errorf("fleet analytics: merchant funnel: %w", err)
	}

	// Sale rows only: refund/chargeback mirror rows share status='completed'
	// (with negative amounts + reversal_kind set) and must not count as sales.
	revenueRows, err := c.pool.Query(ctx,
		`SELECT currency, payments, settled_amount
		   FROM openrails.fleet_revenue_by_currency($1::uuid, $2::timestamptz)`,
		excludeArg, since)
	if err != nil {
		return nil, fmt.Errorf("fleet analytics: revenue: %w", err)
	}
	defer revenueRows.Close()
	for revenueRows.Next() {
		var r FleetCurrencyRevenue
		if err := revenueRows.Scan(&r.Currency, &r.Payments, &r.SettledAmount); err != nil {
			return nil, fmt.Errorf("fleet analytics: scan revenue: %w", err)
		}
		out.Revenue = append(out.Revenue, r)
	}
	if err := revenueRows.Err(); err != nil {
		return nil, fmt.Errorf("fleet analytics: revenue rows: %w", err)
	}

	railRows, err := c.pool.Query(ctx,
		`SELECT rail, succeeded, failed, chargebacks
		   FROM openrails.fleet_rail_health($1::uuid, $2::timestamptz)`,
		excludeArg, since)
	if err != nil {
		return nil, fmt.Errorf("fleet analytics: rail health: %w", err)
	}
	defer railRows.Close()
	for railRows.Next() {
		var r FleetRailHealth
		if err := railRows.Scan(&r.Rail, &r.Succeeded, &r.Failed, &r.Chargebacks); err != nil {
			return nil, fmt.Errorf("fleet analytics: scan rail health: %w", err)
		}
		out.Rails = append(out.Rails, r)
	}
	if err := railRows.Err(); err != nil {
		return nil, fmt.Errorf("fleet analytics: rail rows: %w", err)
	}

	mrrRows, err := c.pool.Query(ctx,
		`SELECT currency, subscriptions, monthly_amount
		   FROM openrails.fleet_mrr_by_currency($1::uuid)`,
		excludeArg)
	if err != nil {
		return nil, fmt.Errorf("fleet analytics: mrr: %w", err)
	}
	defer mrrRows.Close()
	for mrrRows.Next() {
		var r FleetMRR
		if err := mrrRows.Scan(&r.Currency, &r.Subscriptions, &r.MonthlyAmount); err != nil {
			return nil, fmt.Errorf("fleet analytics: scan mrr: %w", err)
		}
		out.MRR = append(out.MRR, r)
	}
	if err := mrrRows.Err(); err != nil {
		return nil, fmt.Errorf("fleet analytics: mrr rows: %w", err)
	}
	return out, nil
}
