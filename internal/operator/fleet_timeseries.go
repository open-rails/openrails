package operator

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
)

// FleetTimeseries returns the weekly fleet trend series (openrails-saas #38)
// through the same 0022 SECURITY DEFINER aggregates — the FleetAnalytics
// snapshot's trend companion, under the same SearchMerchants (#226) doctrine: the CALLER
// gates it behind platform-superadmin authority and audits every request.
// exclude removes one merchant from every series (a hosted platform passes its
// own platform merchant); zero excludes nothing. weeks outside 4..52 falls
// back to 12. Calling without an attached control plane is a wiring error
// (call Attach/AttachWithOptions first).
func FleetTimeseries(ctx context.Context, a *app.App, exclude billing.MerchantID, weeks int) (*billing.FleetSeries, error) {
	cp := Get(a)
	if cp == nil {
		return nil, fmt.Errorf("control plane: no control plane attached (call Attach first)")
	}
	series, err := cp.FleetTimeseries(ctx, exclude, weeks)
	if err != nil {
		return nil, err
	}
	out := &billing.FleetSeries{
		Weeks:  series.Weeks,
		Points: make([]billing.FleetWeeklyPoint, 0, len(series.Points)),
		Volume: make([]billing.FleetWeeklyVolume, 0, len(series.Volume)),
	}
	for _, p := range series.Points {
		out.Points = append(out.Points, billing.FleetWeeklyPoint{
			WeekStart:              p.WeekStart,
			NewMerchants:           p.NewMerchants,
			ActiveMerchants:        p.ActiveMerchants,
			CancelledSubscriptions: p.CancelledSubscriptions,
		})
	}
	for _, v := range series.Volume {
		out.Volume = append(out.Volume, billing.FleetWeeklyVolume{
			WeekStart:     v.WeekStart,
			Currency:      v.Currency,
			Payments:      v.Payments,
			SettledAmount: v.SettledAmount,
		})
	}
	return out, nil
}
