package controlplane

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/pkg/merchant"
)

// FleetMerchantFunnel counts merchants by lifecycle stage (openrails-saas #28):
// provisioned → armed (live PSP declared) → first-revenue → active-in-window.
type FleetMerchantFunnel struct {
	Total         int64 `json:"total"`
	Armed         int64 `json:"armed"`
	FirstRevenue  int64 `json:"first_revenue"`
	ActiveRevenue int64 `json:"active_revenue"`
}

// FleetCurrencyRevenue is one currency's settled window volume, in MICROS.
type FleetCurrencyRevenue struct {
	Currency      string `json:"currency"`
	Payments      int64  `json:"payments"`
	SettledAmount int64  `json:"settled_amount_micros"`
}

// FleetRailHealth is one rail's completed/failed split across the fleet window.
type FleetRailHealth struct {
	Rail      string `json:"rail"`
	Succeeded int64  `json:"succeeded"`
	Failed    int64  `json:"failed"`
}

// FleetMRR is one currency's monthly-normalized recurring run-rate, in MICROS.
type FleetMRR struct {
	Currency      string `json:"currency"`
	Subscriptions int64  `json:"subscriptions"`
	MonthlyAmount int64  `json:"monthly_amount_micros"`
}

// FleetSnapshot is one operator snapshot of the hosted fleet.
type FleetSnapshot struct {
	WindowDays int                    `json:"window_days"`
	Merchants  FleetMerchantFunnel    `json:"merchants"`
	Revenue    []FleetCurrencyRevenue `json:"revenue"`
	Rails      []FleetRailHealth      `json:"rails"`
	MRR        []FleetMRR             `json:"mrr"`
}

// FleetAnalytics returns cross-merchant operator aggregates (openrails-saas
// #28) from the control plane's privileged pool — the fleet view no
// per-merchant RLS scope can compute. Like SearchMerchants (#226) this is a
// sensitive cross-merchant read: the CALLER gates it behind platform-superadmin
// authority and audits every request. exclude removes one merchant from every
// aggregate (a hosted platform passes its own platform merchant); zero excludes
// nothing. windowDays outside 1..365 falls back to 30. Calling without an
// attached control plane is a wiring error (call Attach/AttachWithOptions
// first).
func FleetAnalytics(ctx context.Context, a *app.App, exclude merchant.ID, windowDays int) (*FleetSnapshot, error) {
	cp := Get(a)
	if cp == nil {
		return nil, fmt.Errorf("control plane: no control plane attached (call Attach first)")
	}
	snapshot, err := cp.FleetAnalytics(ctx, exclude, windowDays)
	if err != nil {
		return nil, err
	}
	out := &FleetSnapshot{
		WindowDays: snapshot.WindowDays,
		Merchants: FleetMerchantFunnel{
			Total:         snapshot.Merchants.Total,
			Armed:         snapshot.Merchants.Armed,
			FirstRevenue:  snapshot.Merchants.FirstRevenue,
			ActiveRevenue: snapshot.Merchants.ActiveRevenue,
		},
		Revenue: make([]FleetCurrencyRevenue, 0, len(snapshot.Revenue)),
		Rails:   make([]FleetRailHealth, 0, len(snapshot.Rails)),
		MRR:     make([]FleetMRR, 0, len(snapshot.MRR)),
	}
	for _, r := range snapshot.Revenue {
		out.Revenue = append(out.Revenue, FleetCurrencyRevenue{Currency: r.Currency, Payments: r.Payments, SettledAmount: r.SettledAmount})
	}
	for _, r := range snapshot.Rails {
		out.Rails = append(out.Rails, FleetRailHealth{Rail: r.Rail, Succeeded: r.Succeeded, Failed: r.Failed})
	}
	for _, r := range snapshot.MRR {
		out.MRR = append(out.MRR, FleetMRR{Currency: r.Currency, Subscriptions: r.Subscriptions, MonthlyAmount: r.MonthlyAmount})
	}
	return out, nil
}
