package controlplane

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/pkg/merchant"
)

// FleetWeeklyPoint is one week's fleet movement (openrails-saas #38): merchants
// provisioned, distinct merchants with a settled sale, and cancelled
// subscriptions (the churn proxy). Every week in the window is present,
// zero-filled when quiet.
type FleetWeeklyPoint struct {
	WeekStart              time.Time `json:"week_start"`
	NewMerchants           int64     `json:"new_merchants"`
	ActiveMerchants        int64     `json:"active_merchants"`
	CancelledSubscriptions int64     `json:"cancelled_subscriptions"`
}

// FleetWeeklyVolume is one week's settled sale volume in one currency, in
// MICROS; only weeks×currencies with activity appear.
type FleetWeeklyVolume struct {
	WeekStart     time.Time `json:"week_start"`
	Currency      string    `json:"currency"`
	Payments      int64     `json:"payments"`
	SettledAmount int64     `json:"settled_amount_micros"`
}

// FleetSeries is the windowed weekly trend series for the hosted fleet.
type FleetSeries struct {
	Weeks  int                 `json:"weeks"`
	Points []FleetWeeklyPoint  `json:"points"`
	Volume []FleetWeeklyVolume `json:"volume"`
}

// FleetTimeseries returns the weekly fleet trend series (openrails-saas #38)
// from the control plane's privileged pool — the FleetAnalytics snapshot's
// trend companion, under the same SearchMerchants (#226) doctrine: the CALLER
// gates it behind platform-superadmin authority and audits every request.
// exclude removes one merchant from every series (a hosted platform passes its
// own platform merchant); zero excludes nothing. weeks outside 4..52 falls
// back to 12. Calling without an attached control plane is a wiring error
// (call Attach/AttachWithOptions first).
func FleetTimeseries(ctx context.Context, a *app.App, exclude merchant.ID, weeks int) (*FleetSeries, error) {
	cp := Get(a)
	if cp == nil {
		return nil, fmt.Errorf("control plane: no control plane attached (call Attach first)")
	}
	series, err := cp.FleetTimeseries(ctx, exclude, weeks)
	if err != nil {
		return nil, err
	}
	out := &FleetSeries{
		Weeks:  series.Weeks,
		Points: make([]FleetWeeklyPoint, 0, len(series.Points)),
		Volume: make([]FleetWeeklyVolume, 0, len(series.Volume)),
	}
	for _, p := range series.Points {
		out.Points = append(out.Points, FleetWeeklyPoint{
			WeekStart:              p.WeekStart,
			NewMerchants:           p.NewMerchants,
			ActiveMerchants:        p.ActiveMerchants,
			CancelledSubscriptions: p.CancelledSubscriptions,
		})
	}
	for _, v := range series.Volume {
		out.Volume = append(out.Volume, FleetWeeklyVolume{
			WeekStart:     v.WeekStart,
			Currency:      v.Currency,
			Payments:      v.Payments,
			SettledAmount: v.SettledAmount,
		})
	}
	return out, nil
}
