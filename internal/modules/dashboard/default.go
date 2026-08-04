package dashboard

import "github.com/open-rails/openrails/internal/modules/metrics"

// DefaultWidgets is the seeded template served when a merchant has no saved
// row (#740 default layout): KPI row (mrr / net revenue / churn w/ compare) +
// dunning stat, revenue by week by stream, payment health per rail account,
// new subs by subscriber type — plus usage/credits widgets only when the
// merchant actually has usage activity. Relative ranges keep it current.
// The merchant edits from here; nothing is written until they save.
func DefaultWidgets(hasUsage bool) []Widget {
	last30 := &metrics.QueryRange{Last: "30d"}
	widgets := []Widget{
		{
			ID: "mrr", Title: "MRR", Viz: "stat",
			Query: metrics.Query{Measures: []string{"mrr"}, Range: last30, Compare: "previous_period"},
			Grid:  Grid{X: 0, Y: 0, W: 3, H: 2},
		},
		{
			ID: "net-revenue", Title: "Net revenue", Viz: "stat",
			Query: metrics.Query{Measures: []string{"net_revenue"}, Range: last30, Compare: "previous_period"},
			Grid:  Grid{X: 3, Y: 0, W: 3, H: 2},
		},
		{
			ID: "churn-rate", Title: "Churn rate", Viz: "stat",
			Query: metrics.Query{Measures: []string{"churn_rate"}, Range: last30, Compare: "previous_period"},
			Grid:  Grid{X: 6, Y: 0, W: 3, H: 2},
		},
		{
			ID: "dunning", Title: "Subscriptions in dunning", Viz: "stat",
			Query: metrics.Query{Measures: []string{"subscriptions"},
				Filters: map[string][]string{"status": {"past_due"}}, Range: last30},
			Grid: Grid{X: 9, Y: 0, W: 3, H: 2},
		},
		{
			ID: "revenue-by-stream", Title: "Net revenue by stream", Viz: "area",
			Query: metrics.Query{Measures: []string{"net_revenue"}, By: []string{"time", "stream"},
				Grain: "week", Range: &metrics.QueryRange{Last: "12w"}},
			Grid: Grid{X: 0, Y: 2, W: 6, H: 4},
		},
		{
			ID: "payment-health", Title: "Payment health by rail account", Viz: "table",
			Query: metrics.Query{Measures: []string{"approval_rate", "chargeback_rate"},
				By: []string{"rail_account"}, Range: last30},
			Grid: Grid{X: 6, Y: 2, W: 6, H: 4},
		},
		{
			ID: "new-subscriptions", Title: "New subscriptions by subscriber type", Viz: "bar",
			Query: metrics.Query{Measures: []string{"new_subscriptions"}, By: []string{"time", "subscriber_type"},
				Grain: "week", Range: &metrics.QueryRange{Last: "12w"}},
			Grid: Grid{X: 0, Y: 6, W: 6, H: 4},
		},
	}
	if hasUsage {
		widgets = append(widgets,
			Widget{
				ID: "usage-vs-credits", Title: "Credits sold vs usage revenue", Viz: "line",
				Query: metrics.Query{Measures: []string{"credits_sold", "usage_revenue"}, By: []string{"time"},
					Grain: "week", Range: &metrics.QueryRange{Last: "12w"}},
				Grid: Grid{X: 6, Y: 6, W: 6, H: 4},
			},
			Widget{
				ID: "top-payers", Title: "Top payers by usage revenue", Viz: "table",
				Query: metrics.Query{Measures: []string{"usage_revenue"}, By: []string{"payer"}, Range: last30,
					Order: []metrics.OrderTerm{{Measure: "usage_revenue", Dir: "desc"}}, Limit: intp(10)},
				Grid: Grid{X: 0, Y: 10, W: 6, H: 4},
			},
		)
	}
	return widgets
}

func intp(v int) *int { return &v }
