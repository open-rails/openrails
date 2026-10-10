package dashboard

import "github.com/open-rails/openrails/internal/modules/metrics"

// DefaultWidgets is the template served when a merchant has no saved layout;
// usage widgets appear only when the merchant has usage. Nothing is written
// until the merchant saves.
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
			Query: metrics.Query{Measures: []string{"attempt_failure_rate", "chargeback_rate"},
				By: []string{"psp"}, Range: last30},
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
				ID: "top-customers", Title: "Top customers by usage revenue", Viz: "table",
				Query: metrics.Query{Measures: []string{"usage_revenue"}, By: []string{"customer"}, Range: last30,
					Order: []metrics.OrderTerm{{Measure: "usage_revenue", Dir: "desc"}}, Limit: intp(10)},
				Grid: Grid{X: 0, Y: 10, W: 6, H: 4},
			},
		)
	}
	return widgets
}

func intp(v int) *int { return &v }
