//go:build greenfield && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/openrails/nmimock"
)

// nmiFake is the world's NMI gateway: nmimock plus conversions to this
// suite's shared provider types (card, gate, ledgerEntry, providerCall).
type nmiFake struct{ *nmimock.Mock }

func newNMIFake(now func() time.Time) *nmiFake {
	return &nmiFake{nmimock.NewUnstarted(nmimock.Options{Clock: now, PlanFallback: legacyPlan})}
}

// legacyPlan is a legacy book's monthly 9.99 plan, created at NMI long ago.
func legacyPlan(id string) (nmimock.Plan, bool) {
	if !strings.HasPrefix(id, "legacy_plan_") {
		return nmimock.Plan{}, false
	}
	return nmimock.Plan{ID: id, Name: "Legacy", Amount: "9.99", Days: 30}, true
}

// hold parks matching requests on g, as the Stripe fake does.
func (f *nmiFake) hold(g *gate) *gate {
	f.Intercept(g.match, func(r *http.Request, serve func() *http.Response) (*http.Response, error) {
		var res *http.Response
		if g.served {
			res = serve()
		}
		g.once.Do(func() { close(g.arrived) })
		select {
		case <-g.release:
		case <-r.Context().Done():
			if g.commit && res == nil {
				serve()
			}
			return nil, r.Context().Err()
		}
		if res == nil {
			res = serve()
		}
		return res, nil
	})
	return g
}

func (f *nmiFake) unhold() { f.ClearIntercepts() }

// ledger is the gateway's approved sales for a vault ("" = all).
func (f *nmiFake) ledger(vault string) []ledgerEntry {
	var out []ledgerEntry
	for _, e := range f.Ledger(vault) {
		out = append(out, ledgerEntry{ID: e.TransactionID, Method: e.Last4, Amount: e.Cents, Refunded: e.RefundedCents})
	}
	return out
}

// calls converts the mock's journal to the suite's providerCall.
func calls(in []nmimock.Call) []providerCall {
	out := make([]providerCall, 0, len(in))
	for _, c := range in {
		out = append(out, providerCall{Method: c.Method, Path: c.Path, Form: c.Form})
	}
	return out
}

// customSchedule makes a schedule a custom-amount one (no named plan), as a
// legacy system that set plan_amount directly left it.
func (f *nmiFake) customSchedule(id string) {
	f.EditSchedule(id, func(s *nmimock.Schedule) { s.Custom, s.Plan = true, "custom-"+id })
}

func decimalCents(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}
