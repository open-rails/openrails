package dashboard

import (
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/metrics"
)

func TestDefaultWidgetsValidateForEveryProfile(t *testing.T) {
	for bits := range 32 {
		p := Profile{Subscriptions: bits&1 != 0, CardPayments: bits&2 != 0, Usage: bits&4 != 0, Credits: bits&8 != 0, Checkouts: bits&16 != 0}
		for _, basis := range []billing.AccountingBasis{billing.AccountingAccrual, billing.AccountingCash} {
			widgets := DefaultWidgets(p, basis)
			if verr := ValidateWidgets(widgets); verr != nil {
				t.Errorf("%+v %s: %v", p, basis, verr)
			}
			first := map[billing.AccountingBasis]string{billing.AccountingAccrual: "recognized_revenue", billing.AccountingCash: "cash"}[basis]
			if p.Subscriptions || Empty(p) {
				first = "mrr"
			}
			if widgets[0].Query.Measures[0] != first {
				t.Errorf("%+v %s: the overview starts with %s", p, basis, widgets[0].ID)
			}
		}
	}
}

// A funnel draws one count by one ordered dimension, never money or time.
func TestFunnelShape(t *testing.T) {
	funnel := func(q metrics.Query) Widget {
		q.Range = &metrics.QueryRange{Last: "30d"}
		return Widget{ID: "f", Title: "Funnel", Viz: Funnel, Query: q, Grid: Grid{W: 6, H: 4}}
	}
	if verr := ValidateWidgets([]Widget{funnel(metrics.Query{Measures: []string{"checkouts"}, By: []string{"step"}})}); verr != nil {
		t.Fatalf("checkouts by step: %v", verr)
	}
	for name, q := range map[string]metrics.Query{
		"money":       {Measures: []string{"cash"}, By: []string{"cash_kind"}},
		"over time":   {Measures: []string{"checkouts"}, By: []string{"time", "step"}, Grain: "day"},
		"open values": {Measures: []string{"checkouts"}, By: []string{"country"}},
		"two by":      {Measures: []string{"checkouts"}, By: []string{"step", "outcome"}},
	} {
		verr := ValidateWidgets([]Widget{funnel(q)})
		if verr == nil || verr.Errors[0].Code != "funnel_shape" {
			t.Errorf("%s: %v", name, verr)
		}
	}
}
