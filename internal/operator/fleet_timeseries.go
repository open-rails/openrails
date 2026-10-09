package operator

import (
	"context"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/controlplane"
)

// FleetTimeseries returns the weekly fleet trend series
// as aggregates only — the FleetAnalytics
// snapshot's trend companion, under the same SearchMerchants (#226) doctrine: the CALLER
// gates it behind platform-superadmin authority and audits every request.
// exclude removes one merchant from every series (a hosted platform passes its
// own platform merchant); zero excludes nothing. weeks outside 4..52 is
// refused.
func FleetTimeseries(ctx context.Context, cp *controlplane.ControlPlane, exclude billing.MerchantID, weeks int) (*billing.FleetSeries, error) {
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
			WeekStart:             p.WeekStart,
			NewMerchants:          p.NewMerchants,
			ActiveMerchants:       p.ActiveMerchants,
			CanceledSubscriptions: p.CanceledSubscriptions,
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
