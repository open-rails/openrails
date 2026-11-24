package dashboard

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/metrics"
)

// Profile is what the merchant's data shows it sells.
type Profile = billing.DashboardProfile

// Empty reports a merchant with no activity yet.
func Empty(p Profile) bool { return p == Profile{} }

// Grid sizes: tiles four to a row, charts two, headings and wide charts
// across.
const (
	cols     = 12
	tileW    = 3
	tileH    = 3
	chartW   = 6
	chartH   = 4
	wideH    = 6
	headingH = 1
)

// DefaultWidgets is the health overview served while a merchant has saved no
// layout: KPI tiles, then a section of trend charts for each thing the
// profile shows it sells. Sections without data are left out; an empty
// merchant gets the tiles of a subscription business and no sections. Every
// widget follows the range the viewer picks (its saved range, the last 30
// days, is the default) at a grain that fits it.
func DefaultWidgets(p Profile) []Widget {
	empty := Empty(p)
	subscriptions, cards := p.Subscriptions || empty, p.CardPayments || empty
	var l layout

	if subscriptions {
		l.tile("kpi-mrr", "MRR", "mrr", nil)
		l.tile("kpi-net-new-mrr", "Net new MRR", "mrr_movements", nil)
		l.tile("kpi-subscribers", "Subscribers", "subscribers", nil)
		l.tile("kpi-churn-rate", "Churn rate", "subscriber_churn_rate", nil)
		l.tile("kpi-nrr", "Net revenue retention", "nrr", nil)
		l.tile("kpi-at-risk", "At-risk MRR", "mrr_at_risk", nil)
	}
	l.tile("kpi-cash", "Cash collected", "cash", nil)
	if subscriptions {
		l.tile("kpi-first-try-rebill", "First-try rebill approval", "first_try_rebill_approval_rate", nil)
	}
	if cards {
		l.tile("kpi-authorization-rate", "Authorization rate", "authorization_rate", nil)
		l.tile("kpi-issuer-discretion", "Issuer-discretion decline rate", "issuer_discretion_decline_rate", nil)
		l.tile("kpi-chargeback-ratio", "Visa chargeback ratio (VAMP)", "vamp_ratio", nil)
		l.tile("kpi-refund-rate", "Refund rate", "refund_rate", nil)
	}
	if p.Usage {
		l.tile("kpi-usage-charged", "Usage charged", "usage_charged", map[string][]string{"charge": {"charged"}})
		l.tile("kpi-active-customers", "Active customers", "active_customers", nil)
	}
	if p.Credits {
		l.tile("kpi-credits-sold", "Credits sold", "credits_sold", nil)
	}
	if p.Checkouts {
		l.tile("kpi-checkout-conversion", "Checkout conversion", "checkout_conversion_rate", nil)
	}
	if empty {
		return l.widgets
	}

	l.section("revenue", "Revenue and cash",
		chart("cash-by-kind", "Cash by kind", "bar", series([]string{"cash"}, "cash_kind")),
		chart("cash-by-psp", "Cash by PSP", "donut", metrics.Query{Measures: []string{"cash"}, By: []string{"psp"}}),
	)
	if p.Subscriptions {
		l.section("recurring", "Recurring",
			chart("mrr", "MRR", "line", series([]string{"mrr"})),
			fixed("mrr-movements", "MRR movements by month", "bar", monthly([]string{"mrr_movements"}, "movement")),
			fixed("scheduled-mrr", "Scheduled churn and contraction by effective week", "bar", metrics.Query{Measures: []string{"scheduled_mrr"},
				By: []string{"time", "movement"}, Grain: "week", Range: &metrics.QueryRange{Next: "12w"}}),
			chart("at-risk-by-reason", "At-risk MRR by reason", "area", series([]string{"mrr_at_risk"}, "risk_reason")),
		)
		l.section("retention", "Retention",
			wide(fixed("cohort-retention", "Retention by cohort", Cohort, metrics.Query{Measures: []string{"cohort_retention"},
				By: []string{"cohort", "months_since_start", "basis"}, Range: &metrics.QueryRange{Last: "12m"}})),
			fixed("revenue-retention", "Net and gross revenue retention by month", "line", monthly([]string{"nrr", "grr"})),
			fixed("rebill-rate", "Rebill rate by month", "line", monthly([]string{"rebill_rate"})),
		)
	}
	var health []Widget
	if p.Subscriptions {
		health = append(health,
			chart("rebills-by-outcome", "Renewals by outcome", "bar", series([]string{"rebills"}, "outcome")),
			chart("dunning-recovery", "Dunning recovery rate", "line", series([]string{"dunning_recovery_rate"})),
			chart("at-risk-recovery", "At-risk MRR recovered, by the reason it entered with", "line", series([]string{"at_risk_recovery_rate"}, "risk_reason")),
			fixed("first-try-rebill", "First-try rebill approval by month", "line", monthly([]string{"first_try_rebill_approval_rate"})),
		)
	}
	if p.CardPayments {
		rates := []string{"attempts", "authorization_rate", "issuer_discretion_decline_rate"}
		by := func(id, title, dim string) Widget {
			return chart(id, title, "table", metrics.Query{Measures: rates, By: []string{dim},
				Order: []metrics.OrderTerm{{Measure: "attempts", Dir: "desc"}}, Limit: intp(10)})
		}
		health = append(health,
			chart("vamp-ratio", "Visa VAMP ratio against its levels", "line", metrics.Query{Measures: []string{"vamp_ratio"}, By: []string{"time"}, Grain: "month"}),
			chart("ecm-ratio", "Mastercard ECM ratio against its levels", "line", metrics.Query{Measures: []string{"ecm_ratio"}, By: []string{"time"}, Grain: "month"}),
			chart("chargeback-rate", "Chargeback rate", "line", series([]string{"chargeback_rate"})),
			chart("refund-rate", "Refund rate", "line", series([]string{"refund_rate"})),
			Widget{ID: "dispute-programs", Title: "Dispute programs by PSP and network", Viz: Programs},
			chart("attempts-by-psp", "Attempts by PSP and outcome", "table", metrics.Query{Measures: []string{"attempts"}, By: []string{"psp", "outcome"}}),
			chart("authorization-rates", "Authorization and issuer-discretion decline rate", "line", series([]string{"authorization_rate", "issuer_discretion_decline_rate"})),
			chart("declines-by-cause", "Declines by cause", "bar", metrics.Query{Measures: []string{"attempts"}, By: []string{"time", "decline_cause"}, Grain: "auto",
				Filters: map[string][]string{"outcome": {"declined"}}}),
			by("authorization-by-psp", "Authorization by PSP", "psp"),
			by("authorization-by-brand", "Authorization by card brand", "card_brand"),
			by("authorization-by-bin", "Authorization by BIN", "card_bin"),
			by("authorization-by-token", "Authorization by token type", "token_type"),
			chart("disputes-by-reason", "Disputes by reason", "bar", series([]string{"disputes"}, "dispute_reason")),
			chart("dispute-win-rate", "Dispute win rate", "line", series([]string{"dispute_win_rate"})),
			chart("disputes-by-charge", "Disputes on initial charges and renewals", "bar", series([]string{"disputes"}, "kind")),
			chart("fraud-reports", "Fraud reports by what was done", "bar", series([]string{"fraud_reports"}, "outcome")),
		)
	}
	l.section("payment-health", "Payment health", health...)
	if p.Checkouts {
		top := func(id, title, by string) Widget {
			return chart(id, title, "table", metrics.Query{Measures: []string{"sales", "cash"}, By: []string{by},
				Order: []metrics.OrderTerm{{Measure: "sales", Dir: "desc"}}, Limit: intp(10)})
		}
		l.section("checkouts", "Checkout and customers",
			chart("checkout-funnel", "Checkout funnel", Funnel, metrics.Query{Measures: []string{"checkouts"}, By: []string{"step"}}),
			chart("checkouts-abandoned", "Abandoned checkouts by step", "bar", withFilters(series([]string{"checkouts"}, "step"), map[string][]string{"outcome": {"abandoned"}})),
			chart("checkout-conversion", "Checkout conversion by surface", "line", series([]string{"checkout_conversion_rate"}, "surface")),
			chart("customers-by-type", "Customers by type", "bar", series([]string{"sales_customers"}, "customer_type")),
			chart("repeat-purchase-rate", "Repeat purchase rate", "line", series([]string{"repeat_purchase_rate"})),
			top("sales-by-country", "Sales and cash by country", "country"),
			top("sales-by-language", "Sales and cash by language", "language"),
			top("sales-by-source", "Sales and cash by source", "attribution_source"),
		)
	}
	var credits []Widget
	switch {
	case p.Usage && p.Credits:
		credits = append(credits, chart("usage-vs-credits", "Usage against credits sold", "line", series([]string{"usage_charged", "credits_sold"})))
	case p.Usage:
		credits = append(credits, chart("usage-by-charge", "Usage charged and forgiven", "bar", series([]string{"usage_charged"}, "charge")))
	case p.Credits:
		credits = append(credits, chart("credits-sold", "Credits sold", "line", series([]string{"credits_sold"})))
	}
	if p.Usage {
		credits = append(credits,
			chart("active-customers", "Active customers", "line", series([]string{"active_customers"})),
			chart("top-customers", "Top customers by usage", "table", metrics.Query{Measures: []string{"usage_charged"}, By: []string{"customer"},
				Filters: map[string][]string{"charge": {"charged"}},
				Order:   []metrics.OrderTerm{{Measure: "usage_charged", Dir: "desc"}}, Limit: intp(10)}),
		)
	}
	if p.Credits {
		credits = append(credits, chart("depletion-risk", "Customers at depletion risk", "line", series([]string{"customers_at_depletion_risk"})))
	}
	l.section("credits-usage", "Credits and usage", credits...)
	return l.widgets
}

func withFilters(q metrics.Query, filters map[string][]string) metrics.Query {
	q.Filters = filters
	return q
}

// layout places widgets row by row.
type layout struct {
	widgets []Widget
	y       int
	tiles   int // tiles placed in the current row of tiles
}

func (l *layout) tile(id, title, measure string, filters map[string][]string) {
	if l.tiles == cols/tileW {
		l.y, l.tiles = l.y+tileH, 0
	}
	l.widgets = append(l.widgets, Widget{
		ID: id, Title: title, Viz: "kpi",
		Query: metrics.Query{Measures: []string{measure}, Filters: filters, Range: saved(), Compare: "previous_period"},
		Grid:  Grid{X: l.tiles * tileW, Y: l.y, W: tileW, H: tileH},
	})
	l.tiles++
}

// section is a heading over its charts, two to a row, a wide one across;
// none is no section.
func (l *layout) section(id, title string, charts ...Widget) {
	if len(charts) == 0 {
		return
	}
	if l.tiles > 0 {
		l.y, l.tiles = l.y+tileH, 0
	}
	l.widgets = append(l.widgets, Widget{ID: "section-" + id, Title: title, Viz: Heading, Grid: Grid{X: 0, Y: l.y, W: cols, H: headingH}})
	l.y += headingH
	x := 0
	for _, c := range charts {
		if c.Grid.W == cols || x+chartW > cols {
			if x > 0 {
				l.y, x = l.y+chartH, 0
			}
		}
		if c.Grid.W == cols {
			c.Grid = Grid{X: 0, Y: l.y, W: cols, H: wideH}
			l.widgets = append(l.widgets, c)
			l.y += wideH
			continue
		}
		c.Grid = Grid{X: x, Y: l.y, W: chartW, H: chartH}
		l.widgets = append(l.widgets, c)
		x += chartW
	}
	if x > 0 {
		l.y += chartH
	}
}

func chart(id, title, viz string, q metrics.Query) Widget {
	q.Range = saved()
	return Widget{ID: id, Title: title, Viz: viz, Query: q}
}

// fixed is a chart that keeps its own range, whatever the page's.
func fixed(id, title, viz string, q metrics.Query) Widget {
	return Widget{ID: id, Title: title, Viz: viz, Query: q, FixedRange: true}
}

// wide spans the dashboard.
func wide(w Widget) Widget {
	w.Grid.W = cols
	return w
}

// monthly is measures by month over the last twelve, split by the given
// dimensions.
func monthly(measures []string, by ...string) metrics.Query {
	return metrics.Query{Measures: measures, By: append([]string{"time"}, by...), Grain: "month", Range: &metrics.QueryRange{Last: "12m"}}
}

// series is measures over time at the grain that fits the range, split by
// the given dimensions.
func series(measures []string, by ...string) metrics.Query {
	return metrics.Query{Measures: measures, By: append([]string{"time"}, by...), Grain: "auto"}
}

func saved() *metrics.QueryRange { return &metrics.QueryRange{Last: "30d"} }

func intp(v int) *int { return &v }
