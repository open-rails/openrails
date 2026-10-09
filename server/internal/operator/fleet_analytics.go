package operator

import (
	"context"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server/internal/controlplane"
)

// FleetAnalytics returns cross-merchant operator aggregates —
// the fleet view no per-merchant scope can compute. Its queries
// return aggregates only, never merchant rows. Like
// SearchMerchants (#226) this is a sensitive cross-merchant read: the CALLER
// gates it behind platform-superadmin authority and audits every request.
// exclude removes one merchant from every aggregate (a hosted platform passes
// its own platform merchant); zero excludes nothing. windowDays outside 1..365
// is refused.
func FleetAnalytics(ctx context.Context, cp *controlplane.ControlPlane, exclude billing.MerchantID, windowDays int) (*billing.FleetSnapshot, error) {
	snapshot, err := cp.FleetAnalytics(ctx, exclude, windowDays)
	if err != nil {
		return nil, err
	}
	out := &billing.FleetSnapshot{
		WindowDays: snapshot.WindowDays,
		Merchants: billing.FleetMerchantFunnel{
			Total:         snapshot.Merchants.Total,
			Armed:         snapshot.Merchants.Armed,
			FirstRevenue:  snapshot.Merchants.FirstRevenue,
			ActiveRevenue: snapshot.Merchants.ActiveRevenue,
		},
		Revenue: make([]billing.FleetCurrencyRevenue, 0, len(snapshot.Revenue)),
		Rails:   make([]billing.FleetRailHealth, 0, len(snapshot.Rails)),
		MRR:     make([]billing.FleetMRR, 0, len(snapshot.MRR)),
	}
	for _, r := range snapshot.Revenue {
		out.Revenue = append(out.Revenue, billing.FleetCurrencyRevenue{Currency: r.Currency, Payments: r.Payments, SettledAmount: r.SettledAmount})
	}
	for _, r := range snapshot.Rails {
		out.Rails = append(out.Rails, billing.FleetRailHealth{Rail: r.Rail, Succeeded: r.Succeeded, Failed: r.Failed, Chargebacks: r.Chargebacks})
	}
	for _, r := range snapshot.MRR {
		out.MRR = append(out.MRR, billing.FleetMRR{Currency: r.Currency, Subscriptions: r.Subscriptions, MonthlyAmount: r.MonthlyAmount})
	}
	return out, nil
}
