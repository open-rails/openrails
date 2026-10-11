package dashboard

import "github.com/open-rails/openrails/internal/modules/metrics"

// DefaultWidgets is the template served when a merchant has no saved layout;
// usage widgets appear only when the merchant has usage. Nothing is written
// until the merchant saves. The first row is today against yesterday; every
// other widget follows the range the viewer picks (its saved range is the
// default).
func DefaultWidgets(hasUsage bool) []Widget {
	last30 := &metrics.QueryRange{Last: "30d"}
	last12w := &metrics.QueryRange{Last: "12w"}
	today := func(id, title string, x, w int, measure string, filters map[string][]string) Widget {
		return Widget{
			ID: id, Title: title, Viz: "stat", FixedRange: true,
			Query: metrics.Query{Measures: []string{measure}, Filters: filters,
				Range: &metrics.QueryRange{Last: "1d"}, Compare: "previous_period"},
			Grid: Grid{X: x, Y: 0, W: w, H: 2},
		}
	}
	stat := func(id, title string, x, y, w int, measure string, filters map[string][]string) Widget {
		return Widget{
			ID: id, Title: title, Viz: "stat",
			Query: metrics.Query{Measures: []string{measure}, Filters: filters, Range: last30, Compare: "previous_period"},
			Grid:  Grid{X: x, Y: y, W: w, H: 2},
		}
	}
	widgets := []Widget{
		today("today-new", "New subscriptions today", 0, 3, "new_subscriptions", nil),
		today("today-renewals", "Renewals today", 3, 3, "payment_count", map[string][]string{"attempt_kind": {"renewal"}}),
		today("today-cancellations", "Cancellations today", 6, 2, "cancellations", nil),
		today("today-failed-purchases", "Failed purchases today", 8, 2, "failed_attempts", map[string][]string{"kind": {"initial"}}),
		today("today-net", "Net change today", 10, 2, "net_new_subscriptions", nil),

		stat("active-subscribers", "Active subscribers", 0, 2, 3, "subscriptions", map[string][]string{"status": {"active"}}),
		stat("net-new", "Net new subscribers", 3, 2, 3, "net_new_subscriptions", nil),
		stat("mrr", "MRR", 6, 2, 3, "mrr", nil),
		stat("net-revenue", "Net revenue", 9, 2, 3, "net_revenue", nil),
		stat("churn-rate", "Churn rate", 0, 4, 4, "churn_rate", nil),
		stat("failed-renewals", "Failed renewals", 4, 4, 4, "rebill_first_failures", nil),
		{
			ID: "dunning", Title: "Subscriptions in dunning", Viz: "stat",
			Query: metrics.Query{Measures: []string{"subscriptions"},
				Filters: map[string][]string{"status": {"past_due"}}, Range: last30},
			Grid: Grid{X: 8, Y: 4, W: 4, H: 2},
		},
		{
			ID: "new-vs-canceled", Title: "New vs canceled by day", Viz: "line",
			Query: metrics.Query{Measures: []string{"new_subscriptions", "cancellations"}, By: []string{"time"},
				Grain: "day", Range: last30},
			Grid: Grid{X: 0, Y: 6, W: 6, H: 4},
		},
		{
			ID: "cancellations-by-type", Title: "Cancellations by type", Viz: "bar",
			Query: metrics.Query{Measures: []string{"cancellations"}, By: []string{"time", "cancel_type"},
				Grain: "week", Range: last12w},
			Grid: Grid{X: 6, Y: 6, W: 6, H: 4},
		},
		{
			ID: "revenue-by-rail", Title: "Net revenue by rail", Viz: "donut",
			Query: metrics.Query{Measures: []string{"net_revenue"}, By: []string{"rail"}, Range: last30},
			Grid:  Grid{X: 0, Y: 10, W: 6, H: 4},
		},
		{
			ID: "revenue-by-stream", Title: "Net revenue by stream", Viz: "area",
			Query: metrics.Query{Measures: []string{"net_revenue"}, By: []string{"time", "stream"},
				Grain: "week", Range: last12w},
			Grid: Grid{X: 6, Y: 10, W: 6, H: 4},
		},
		{
			ID: "payment-health", Title: "Payment health by rail account", Viz: "table",
			Query: metrics.Query{Measures: []string{"attempt_failure_rate", "chargeback_rate"},
				By: []string{"psp"}, Range: last30},
			Grid: Grid{X: 0, Y: 14, W: 6, H: 4},
		},
		{
			ID: "new-subscriptions", Title: "New subscriptions by subscriber type", Viz: "bar",
			Query: metrics.Query{Measures: []string{"new_subscriptions"}, By: []string{"time", "subscriber_type"},
				Grain: "week", Range: last12w},
			Grid: Grid{X: 6, Y: 14, W: 6, H: 4},
		},
	}
	if hasUsage {
		widgets = append(widgets,
			Widget{
				ID: "usage-vs-credits", Title: "Credits sold vs usage revenue", Viz: "line",
				Query: metrics.Query{Measures: []string{"credits_sold", "usage_revenue"}, By: []string{"time"},
					Grain: "week", Range: last12w},
				Grid: Grid{X: 0, Y: 18, W: 6, H: 4},
			},
			Widget{
				ID: "top-customers", Title: "Top customers by usage revenue", Viz: "table",
				Query: metrics.Query{Measures: []string{"usage_revenue"}, By: []string{"customer"}, Range: last30,
					Order: []metrics.OrderTerm{{Measure: "usage_revenue", Dir: "desc"}}, Limit: intp(10)},
				Grid: Grid{X: 6, Y: 18, W: 6, H: 4},
			},
		)
	}
	return widgets
}

func intp(v int) *int { return &v }
