package dashboard

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/metrics"
)

// Profile is what the merchant's data shows it sells.
type Profile = billing.DashboardProfile

// Empty reports a merchant with no activity yet.
func Empty(p Profile) bool { return p == Profile{} }

// Grid sizes: tiles four to a row, charts two, headings across.
const (
	cols     = 12
	tileW    = 3
	tileH    = 3
	chartW   = 6
	chartH   = 4
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

	l.tile("kpi-cash", "Cash collected", "cash", nil)
	if subscriptions {
		l.tile("kpi-subscriptions", "Subscriptions", "subscriptions", nil)
		l.tile("kpi-new-subscriptions", "New subscriptions", "new_subscriptions", nil)
		l.tile("kpi-cancellations", "Cancellations", "cancellations", nil)
		l.tile("kpi-dunning", "Renewals in dunning", "rebills", map[string][]string{"outcome": {"open"}})
		l.tile("kpi-first-try-rebill", "First-try rebill approval", "first_try_rebill_approval_rate", nil)
	}
	if cards {
		l.tile("kpi-authorization-rate", "Authorization rate", "authorization_rate", nil)
		l.tile("kpi-issuer-discretion", "Issuer-discretion decline rate", "issuer_discretion_decline_rate", nil)
		l.tile("kpi-chargeback-rate", "Chargeback rate", "chargeback_rate", nil)
	}
	if p.Usage {
		l.tile("kpi-usage-charged", "Usage charged", "usage_charged", map[string][]string{"charge": {"charged"}})
		l.tile("kpi-active-customers", "Active customers", "active_customers", nil)
	}
	if p.Credits {
		l.tile("kpi-credits-sold", "Credits sold", "credits_sold", nil)
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
			chart("new-vs-canceled", "New against canceled subscriptions", "line", series([]string{"new_subscriptions", "cancellations"})),
			chart("cancellations-by-type", "Cancellations by type", "bar", series([]string{"cancellations"}, "cancel_type")),
		)
		l.section("retention", "Retention",
			chart("rebill-rate", "Rebill rate", "line", series([]string{"rebill_rate"})),
		)
	}
	var health []Widget
	if p.Subscriptions {
		health = append(health,
			chart("rebills-by-outcome", "Renewals by outcome", "bar", series([]string{"rebills"}, "outcome")),
			chart("dunning-recovery", "Dunning recovery rate", "line", series([]string{"dunning_recovery_rate"})),
			chart("first-try-rebill", "First-try rebill approval", "line", series([]string{"first_try_rebill_approval_rate"})),
		)
	}
	if p.CardPayments {
		rates := []string{"attempts", "authorization_rate", "issuer_discretion_decline_rate"}
		by := func(id, title, dim string) Widget {
			return chart(id, title, "table", metrics.Query{Measures: rates, By: []string{dim},
				Order: []metrics.OrderTerm{{Measure: "attempts", Dir: "desc"}}, Limit: intp(10)})
		}
		health = append(health,
			chart("refund-chargeback-rate", "Refund and chargeback rate", "line", series([]string{"refund_rate", "chargeback_rate"})),
			chart("attempts-by-psp", "Attempts by PSP and outcome", "table", metrics.Query{Measures: []string{"attempts"}, By: []string{"psp", "outcome"}}),
			chart("authorization-rates", "Authorization and issuer-discretion decline rate", "line", series([]string{"authorization_rate", "issuer_discretion_decline_rate"})),
			chart("declines-by-cause", "Declines by cause", "bar", metrics.Query{Measures: []string{"attempts"}, By: []string{"time", "decline_cause"}, Grain: "auto",
				Filters: map[string][]string{"outcome": {"declined"}}}),
			by("authorization-by-psp", "Authorization by PSP", "psp"),
			by("authorization-by-brand", "Authorization by card brand", "card_brand"),
			by("authorization-by-bin", "Authorization by BIN", "card_bin"),
			by("authorization-by-token", "Authorization by token type", "token_type"),
		)
	}
	l.section("payment-health", "Payment health", health...)
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

// section is a heading over its charts, two to a row; none is no section.
func (l *layout) section(id, title string, charts ...Widget) {
	if len(charts) == 0 {
		return
	}
	if l.tiles > 0 {
		l.y, l.tiles = l.y+tileH, 0
	}
	l.widgets = append(l.widgets, Widget{ID: "section-" + id, Title: title, Viz: Heading, Grid: Grid{X: 0, Y: l.y, W: cols, H: headingH}})
	l.y += headingH
	for i, c := range charts {
		c.Grid = Grid{X: (i % 2) * chartW, Y: l.y + (i/2)*chartH, W: chartW, H: chartH}
		l.widgets = append(l.widgets, c)
	}
	l.y += (len(charts) + 1) / 2 * chartH
}

func chart(id, title, viz string, q metrics.Query) Widget {
	q.Range = saved()
	return Widget{ID: id, Title: title, Viz: viz, Query: q}
}

// series is measures over time at the grain that fits the range, split by
// the given dimensions.
func series(measures []string, by ...string) metrics.Query {
	return metrics.Query{Measures: measures, By: append([]string{"time"}, by...), Grain: "auto"}
}

func saved() *metrics.QueryRange { return &metrics.QueryRange{Last: "30d"} }

func intp(v int) *int { return &v }
