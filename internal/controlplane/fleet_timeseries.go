package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/open-rails/openrails/pkg/merchant"
)

// Fleet timeseries (openrails-saas #38): the trend companion to FleetAnalytics
// — weekly buckets over the same truth tables, on the same privileged
// control-plane pool, under the same SearchMerchants (#226) doctrine: the
// CALLER gates (platform superadmin) and audits every request. Buckets are
// ISO weeks (Postgres date_trunc('week', ...), Monday-start, UTC), computed on
// request — no rollup storage at current fleet scale.

// FleetWeeklyPoint is one week's fleet movement: merchants provisioned, the
// distinct merchants with a settled sale, and cancelled subscriptions (the
// churn proxy until a richer signal exists).
type FleetWeeklyPoint struct {
	WeekStart              time.Time
	NewMerchants           int64
	ActiveMerchants        int64
	CancelledSubscriptions int64
}

// FleetWeeklyVolume is one week's settled sale volume in one currency, in
// MICROS. Sale rows only — reversal mirror rows never count.
type FleetWeeklyVolume struct {
	WeekStart     time.Time
	Currency      string
	Payments      int64
	SettledAmount int64
}

// FleetTimeseriesResult is the whole windowed series. Points carries every
// week in the window (zero-filled when nothing happened) so trend rendering
// never has gaps; Volume carries only weeks×currencies with activity.
type FleetTimeseriesResult struct {
	Weeks  int
	Points []FleetWeeklyPoint
	Volume []FleetWeeklyVolume
}

// FleetTimeseries aggregates the weekly fleet series over the trailing window.
// exclude removes one merchant from every series (the platform's own
// self-billing book); zero excludes nothing. weeks outside 4..52 falls back
// to 12.
func (c *ControlPlane) FleetTimeseries(ctx context.Context, exclude merchant.ID, weeks int) (*FleetTimeseriesResult, error) {
	if c == nil || c.pool == nil {
		return nil, errors.New("controlplane: pgx pool unavailable for fleet timeseries")
	}
	if weeks < 4 || weeks > 52 {
		weeks = 12
	}
	since := time.Now().UTC().AddDate(0, 0, -7*(weeks-1))
	var excludeArg any
	if !exclude.IsZero() {
		excludeArg = exclude.UUID()
	}

	// #824 SWEEP: the aggregates below read RLS-bearing tables (payments,
	// subscriptions) on the base pool with no app.merchant_id, so under the
	// production openrails_app role every series is empty — see the same note on
	// FleetAnalytics.
	//
	// Canonical week list from Postgres so bucket alignment can never drift
	// from the aggregates' date_trunc semantics.
	weekRows, err := c.pool.Query(ctx, `
		SELECT generate_series(
		  date_trunc('week', $1::timestamptz),
		  date_trunc('week', now()),
		  interval '7 days')
	`, since)
	if err != nil {
		return nil, fmt.Errorf("fleet timeseries: weeks: %w", err)
	}
	defer weekRows.Close()
	out := &FleetTimeseriesResult{Weeks: weeks}
	index := map[time.Time]int{}
	for weekRows.Next() {
		var week time.Time
		if err := weekRows.Scan(&week); err != nil {
			return nil, fmt.Errorf("fleet timeseries: scan week: %w", err)
		}
		week = week.UTC()
		index[week] = len(out.Points)
		out.Points = append(out.Points, FleetWeeklyPoint{WeekStart: week})
	}
	if err := weekRows.Err(); err != nil {
		return nil, fmt.Errorf("fleet timeseries: week rows: %w", err)
	}

	fill := func(query string, assign func(point *FleetWeeklyPoint, count int64)) error {
		rows, err := c.pool.Query(ctx, query, excludeArg, since)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var week time.Time
			var count int64
			if err := rows.Scan(&week, &count); err != nil {
				return err
			}
			if i, ok := index[week.UTC()]; ok {
				assign(&out.Points[i], count)
			}
		}
		return rows.Err()
	}

	if err := fill(`
		SELECT date_trunc('week', created_at), count(*)
		  FROM openrails.merchants
		 WHERE deleted_at IS NULL AND status = 'active'
		   AND created_at >= date_trunc('week', $2::timestamptz)
		   AND ($1::uuid IS NULL OR id <> $1)
		 GROUP BY 1
	`, func(p *FleetWeeklyPoint, n int64) { p.NewMerchants = n }); err != nil {
		return nil, fmt.Errorf("fleet timeseries: new merchants: %w", err)
	}
	if err := fill(`
		SELECT date_trunc('week', purchased_at), count(DISTINCT merchant_id)
		  FROM openrails.payments
		 WHERE status = 'completed' AND reversal_kind IS NULL
		   AND purchased_at >= date_trunc('week', $2::timestamptz)
		   AND ($1::uuid IS NULL OR merchant_id <> $1)
		 GROUP BY 1
	`, func(p *FleetWeeklyPoint, n int64) { p.ActiveMerchants = n }); err != nil {
		return nil, fmt.Errorf("fleet timeseries: active merchants: %w", err)
	}
	if err := fill(`
		SELECT date_trunc('week', cancelled_at), count(*)
		  FROM openrails.subscriptions
		 WHERE cancelled_at IS NOT NULL
		   AND cancelled_at >= date_trunc('week', $2::timestamptz)
		   AND ($1::uuid IS NULL OR merchant_id <> $1)
		 GROUP BY 1
	`, func(p *FleetWeeklyPoint, n int64) { p.CancelledSubscriptions = n }); err != nil {
		return nil, fmt.Errorf("fleet timeseries: cancelled subscriptions: %w", err)
	}

	volumeRows, err := c.pool.Query(ctx, `
		SELECT date_trunc('week', purchased_at), currency, count(*), COALESCE(sum(amount), 0)
		  FROM openrails.payments
		 WHERE status = 'completed' AND reversal_kind IS NULL
		   AND purchased_at >= date_trunc('week', $2::timestamptz)
		   AND ($1::uuid IS NULL OR merchant_id <> $1)
		 GROUP BY 1, 2 ORDER BY 1, 2
	`, excludeArg, since)
	if err != nil {
		return nil, fmt.Errorf("fleet timeseries: volume: %w", err)
	}
	defer volumeRows.Close()
	for volumeRows.Next() {
		var v FleetWeeklyVolume
		if err := volumeRows.Scan(&v.WeekStart, &v.Currency, &v.Payments, &v.SettledAmount); err != nil {
			return nil, fmt.Errorf("fleet timeseries: scan volume: %w", err)
		}
		v.WeekStart = v.WeekStart.UTC()
		out.Volume = append(out.Volume, v)
	}
	if err := volumeRows.Err(); err != nil {
		return nil, fmt.Errorf("fleet timeseries: volume rows: %w", err)
	}
	return out, nil
}
