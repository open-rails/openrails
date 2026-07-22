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
// cross-merchant read on the privileged control-plane pool — no per-merchant
// RLS scope could compute a fleet view — and the CALLER is responsible for
// gating (platform superadmin) and auditing each request.

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
type FleetRailHealth struct {
	Rail      string
	Succeeded int64
	Failed    int64
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

	out := &FleetAnalytics{WindowDays: windowDays}
	if err := c.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE EXISTS (
		           SELECT 1 FROM openrails.psps p
		            WHERE p.merchant_id = m.id AND p.replaced_at IS NULL)),
		       count(*) FILTER (WHERE EXISTS (
		           SELECT 1 FROM openrails.payments pay
		            WHERE pay.merchant_id = m.id AND pay.status = 'completed')),
		       count(*) FILTER (WHERE EXISTS (
		           SELECT 1 FROM openrails.payments pay
		            WHERE pay.merchant_id = m.id AND pay.status = 'completed'
		              AND pay.purchased_at >= $2))
		  FROM openrails.merchants m
		 WHERE m.deleted_at IS NULL AND m.status = 'active'
		   AND ($1::uuid IS NULL OR m.id <> $1)
	`, excludeArg, since).Scan(
		&out.Merchants.Total, &out.Merchants.Armed,
		&out.Merchants.FirstRevenue, &out.Merchants.ActiveRevenue,
	); err != nil {
		return nil, fmt.Errorf("fleet analytics: merchant funnel: %w", err)
	}

	revenueRows, err := c.pool.Query(ctx, `
		SELECT currency, count(*), COALESCE(sum(amount), 0)
		  FROM openrails.payments
		 WHERE status = 'completed' AND purchased_at >= $2
		   AND ($1::uuid IS NULL OR merchant_id <> $1)
		 GROUP BY currency ORDER BY currency
	`, excludeArg, since)
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

	railRows, err := c.pool.Query(ctx, `
		SELECT rail,
		       count(*) FILTER (WHERE status = 'completed'),
		       count(*) FILTER (WHERE status = 'failed')
		  FROM openrails.payments
		 WHERE purchased_at >= $2 AND status IN ('completed', 'failed')
		   AND ($1::uuid IS NULL OR merchant_id <> $1)
		 GROUP BY rail ORDER BY rail
	`, excludeArg, since)
	if err != nil {
		return nil, fmt.Errorf("fleet analytics: rail health: %w", err)
	}
	defer railRows.Close()
	for railRows.Next() {
		var r FleetRailHealth
		if err := railRows.Scan(&r.Rail, &r.Succeeded, &r.Failed); err != nil {
			return nil, fmt.Errorf("fleet analytics: scan rail health: %w", err)
		}
		out.Rails = append(out.Rails, r)
	}
	if err := railRows.Err(); err != nil {
		return nil, fmt.Errorf("fleet analytics: rail rows: %w", err)
	}

	mrrRows, err := c.pool.Query(ctx, `
		SELECT pr.currency, count(*),
		       COALESCE(sum((pr.amount::numeric * 720 / pr.access_duration_hours)::bigint), 0)
		  FROM openrails.subscriptions s
		  JOIN openrails.prices pr ON pr.id = s.price_id
		 WHERE s.status = 'active' AND pr.auto_renew AND pr.access_duration_hours > 0
		   AND ($1::uuid IS NULL OR s.merchant_id <> $1)
		 GROUP BY pr.currency ORDER BY pr.currency
	`, excludeArg)
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
