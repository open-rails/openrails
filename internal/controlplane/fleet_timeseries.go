package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Fleet timeseries (openrails-saas #38): the trend companion to FleetAnalytics
// — weekly buckets over the same truth tables, through the same 0022
// SECURITY DEFINER aggregates, under the same SearchMerchants (#226) doctrine: the
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

// FleetWeeklyVolume is one week's settled sale volume in one currency, in the
// currency's native units. Sale rows only — reversal mirror rows never count.
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
	var excludeArg *uuid.UUID
	if !exclude.IsZero() {
		id := exclude.UUID()
		excludeArg = &id
	}

	// or#861: the aggregates over merchant-owned tables (payments, subscriptions)
	// go through migration 0022's SECURITY DEFINER readers — read on the base
	// pool they were silently empty under the since-removed RLS.
	// The week list and the new-merchant series stay ordinary queries:
	// generate_series touches no table, and billing.merchants is the
	// global directory.
	//
	// Canonical week list from Postgres so bucket alignment can never drift
	// from the aggregates' date_trunc semantics.
	q := gen.New(c.pool)
	weekStarts, err := q.FleetWeeks(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("fleet timeseries: weeks: %w", err)
	}
	out := &FleetTimeseriesResult{Weeks: weeks}
	index := map[time.Time]int{}
	for _, week := range weekStarts {
		week = week.UTC()
		index[week] = len(out.Points)
		out.Points = append(out.Points, FleetWeeklyPoint{WeekStart: week})
	}
	assign := func(week time.Time, set func(point *FleetWeeklyPoint)) {
		if i, ok := index[week.UTC()]; ok {
			set(&out.Points[i])
		}
	}

	newMerchants, err := q.FleetWeeklyNewMerchants(ctx, gen.FleetWeeklyNewMerchantsParams{ExcludeMerchantID: excludeArg, Since: since})
	if err != nil {
		return nil, fmt.Errorf("fleet timeseries: new merchants: %w", err)
	}
	for _, r := range newMerchants {
		assign(r.WeekStart, func(p *FleetWeeklyPoint) { p.NewMerchants = r.Merchants })
	}
	active, err := q.FleetWeeklyActiveMerchants(ctx, gen.FleetWeeklyActiveMerchantsParams{ExcludeMerchantID: excludeArg, Since: since})
	if err != nil {
		return nil, fmt.Errorf("fleet timeseries: active merchants: %w", err)
	}
	for _, r := range active {
		assign(r.WeekStart, func(p *FleetWeeklyPoint) { p.ActiveMerchants = r.Merchants })
	}
	cancelled, err := q.FleetWeeklyCancelledSubscriptions(ctx, gen.FleetWeeklyCancelledSubscriptionsParams{ExcludeMerchantID: excludeArg, Since: since})
	if err != nil {
		return nil, fmt.Errorf("fleet timeseries: cancelled subscriptions: %w", err)
	}
	for _, r := range cancelled {
		assign(r.WeekStart, func(p *FleetWeeklyPoint) { p.CancelledSubscriptions = r.Cancellations })
	}

	volume, err := q.FleetWeeklyVolume(ctx, gen.FleetWeeklyVolumeParams{ExcludeMerchantID: excludeArg, Since: since})
	if err != nil {
		return nil, fmt.Errorf("fleet timeseries: volume: %w", err)
	}
	for _, v := range volume {
		out.Volume = append(out.Volume, FleetWeeklyVolume{WeekStart: v.WeekStart.UTC(), Currency: v.Currency, Payments: v.Payments, SettledAmount: v.SettledAmount})
	}
	return out, nil
}
