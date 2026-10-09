package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
)

// Fleet timeseries: the trend companion to FleetAnalytics
// — weekly buckets over the same truth tables, aggregates only, under the
// same SearchMerchants (#226) doctrine: the
// CALLER gates (platform superadmin) and audits every request. Buckets are
// ISO weeks (Postgres date_trunc('week', ...), Monday-start, UTC), computed on
// request — no rollup storage at current fleet scale.

// FleetWeeklyPoint is one week's fleet movement: merchants provisioned, the
// distinct merchants with a settled sale, and canceled subscriptions (the
// churn proxy until a richer signal exists).
type FleetWeeklyPoint struct {
	WeekStart             time.Time
	NewMerchants          int64
	ActiveMerchants       int64
	CanceledSubscriptions int64
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
// self-billing book); zero excludes nothing. weeks outside 4..52 is refused
// (billing.ErrInvalid).
func (c *ControlPlane) FleetTimeseries(ctx context.Context, exclude billing.MerchantID, weeks int) (*FleetTimeseriesResult, error) {
	if weeks < 4 || weeks > 52 {
		return nil, invalidRange("weeks", 4, 52)
	}
	if c == nil || c.pool == nil {
		return nil, errors.New("controlplane: pgx pool unavailable for fleet timeseries")
	}
	since := time.Now().UTC().AddDate(0, 0, -7*(weeks-1))
	var excludeArg *uuid.UUID
	if !exclude.IsZero() {
		id := exclude.UUID()
		excludeArg = &id
	}

	// The aggregates over merchant-owned tables (payments, subscriptions) return
	// counts only. The week list touches no table, and billing.merchants is the
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
	canceled, err := q.FleetWeeklyCanceledSubscriptions(ctx, gen.FleetWeeklyCanceledSubscriptionsParams{ExcludeMerchantID: excludeArg, Since: since})
	if err != nil {
		return nil, fmt.Errorf("fleet timeseries: canceled subscriptions: %w", err)
	}
	for _, r := range canceled {
		assign(r.WeekStart, func(p *FleetWeeklyPoint) { p.CanceledSubscriptions = r.Cancellations })
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

// invalidRange refuses an out-of-range argument instead of substituting a default.
func invalidRange(name string, low, high int) error {
	return &billing.StatusError{Status: http.StatusBadRequest, ErrorDetails: billing.ErrorDetails{
		Type: "invalid_request_error", Code: "invalid_param", Message: fmt.Sprintf("%s must be between %d and %d", name, low, high),
	}}
}
