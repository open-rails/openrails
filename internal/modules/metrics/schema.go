package metrics

import (
	"slices"

	"github.com/open-rails/openrails/billing"
)

// SchemaDoc is the GET /v1/metrics/schema payload: the registry as
// the LLM-legible context document. One source of truth — the same registry
// drives enforcement, so this cannot drift.
type SchemaDoc = billing.MetricsSchema

// SchemaMeasure is one requestable measure.
type SchemaMeasure = billing.MetricsMeasure

// SchemaDimension is one group-by/filter axis.
type SchemaDimension = billing.MetricsDimension

// SchemaExample pairs a natural-language intent with the query JSON that
// answers it.
type SchemaExample = billing.MetricsExample

// SchemaLimits are the engine clamps.
type SchemaLimits = billing.MetricsLimits

func intp(v int) *int { return &v }

// Schema builds the registry dump.
func Schema() SchemaDoc {
	doc := SchemaDoc{
		Grains:   Grains,
		Deferred: Deferred,
		Caveats:  Caveats,
		Limits:   SchemaLimits{MaxBuckets: MaxBuckets, MaxLimit: MaxLimit},
		QueryShape: `POST body: {"measures":[...], "by":[dims incl "time"], "grain":"auto|day|week|month|quarter|year", ` +
			`"range":{"from":"YYYY-MM-DD","to":"YYYY-MM-DD"} or {"last":"7d"}, "filters":{dim:[values]}, ` +
			`"order":[{"measure"|"dimension":name,"dir":"asc|desc"}], "limit":N, "compare":"previous_period|previous_year"}. ` +
			`Dates are inclusive days in the merchant's accounting timezone (RFC3339 accepted, [from,to)); "last" is a relative trailing window ending today ` +
			`(Nd|Nw|Nm|Ny, e.g. "7d" = past 7 days incl. today — prefer it for recurring/saved queries so they stay current). ` +
			`grain "auto" is day up to 45 days, week up to 26 weeks, then month. ` +
			`A funnel measure (attempts, purchases, rebills, payments) splits by its outcome or kind dimension; its rates are ratio measures over it. ` +
			`Unknown keys/names are 400s with corrective errors; a removed measure's error names its replacement. ` +
			`Response: {columns, rows, grain, range, compare_rows?}; time series zero-fill every bucket.`,
	}
	for i := range Measures {
		m := &Measures[i]
		if m.Internal {
			continue
		}
		thresholds := m.Thresholds
		if thresholds == nil {
			thresholds = []billing.MetricsThreshold{}
		}
		var dimValues map[string][]string
		for _, d := range m.Dims {
			if v := m.valuesOf(d); v != nil && !slices.Equal(v, dimensionByName[d].Values) {
				if dimValues == nil {
					dimValues = map[string][]string{}
				}
				dimValues[d] = v
			}
		}
		doc.Measures = append(doc.Measures, SchemaMeasure{
			Name: m.Name, Class: m.Class, Unit: m.Unit,
			Group: m.Group, Direction: m.Direction, Thresholds: thresholds,
			Description: m.Description, Formula: m.Formula,
			Dims: m.Dims, DimValues: dimValues, Money: m.Money,
		})
	}
	for i := range Dimensions {
		d := &Dimensions[i]
		doc.Dimensions = append(doc.Dimensions, SchemaDimension{Name: d.Name, Description: d.Description, Values: d.Values})
	}
	doc.Examples = []SchemaExample{
		{
			Intent: "count of users who canceled per day, for the past 7 days",
			Query: Query{
				Measures: []string{"cancellations"},
				By:       []string{"time"},
				Grain:    "day",
				Range:    &QueryRange{Last: "7d"},
			},
		},
		{
			Intent: "KPI row: MRR, cash collected and churn this month vs the same month last year",
			Query: Query{
				Measures: []string{"mrr", "cash", "churn_rate"},
				Range:    &QueryRange{From: "2026-06-01", To: "2026-06-30"},
				Compare:  "previous_year",
			},
		},
		{
			Intent: "cash by kind over the last quarter: payments, refunds and chargebacks (signed), at a grain that fits",
			Query: Query{
				Measures: []string{"cash"},
				By:       []string{"time", "cash_kind"},
				Grain:    "auto",
				Range:    &QueryRange{Last: "90d"},
			},
		},
		{
			Intent: "payment health per PSP: authorization, issuer-discretion decline, refund and chargeback rates",
			Query: Query{
				Measures: []string{"authorization_rate", "issuer_discretion_decline_rate", "refund_rate", "chargeback_rate"},
				By:       []string{"psp"},
				Range:    &QueryRange{From: "2026-06-01", To: "2026-06-30"},
			},
		},
		{
			Intent: "refund and chargeback rates by week",
			Query: Query{
				Measures: []string{"refund_rate", "chargeback_rate"},
				By:       []string{"time"},
				Grain:    "week",
				Range:    &QueryRange{Last: "12w"},
			},
		},
		{
			Intent: "top 10 customers by usage charged",
			Query: Query{
				Measures: []string{"usage_charged"},
				By:       []string{"customer"},
				Range:    &QueryRange{From: "2026-06-01", To: "2026-06-30"},
				Filters:  map[string][]string{"charge": {"charged"}},
				Order:    []OrderTerm{{Measure: "usage_charged", Dir: "desc"}},
				Limit:    intp(10),
			},
		},
		{
			Intent: "one customer's usage this month by rate card and charge (failed usage forgiven apart)",
			Query: Query{
				Measures: []string{"usage_charged"},
				By:       []string{"rate_card", "charge"},
				Range:    &QueryRange{From: "2026-06-01", To: "2026-06-30"},
				Filters:  map[string][]string{"customer": {"0b9f6d3e-5c7a-4c1e-9f2a-6d8e1b2c3d4e"}},
			},
		},
		{
			Intent: "findings that need a person now, by group (the console's bell)",
			Query: Query{
				Measures: []string{"open_findings"},
				By:       []string{"finding_group"},
				Range:    &QueryRange{Last: "1d"},
				Filters:  map[string][]string{"finding_status": {"requires_review"}},
			},
		},
		{
			Intent: "new-card declines per day and PSP, by decline reason",
			Query: Query{
				Measures: []string{"attempts"},
				By:       []string{"time", "psp", "reason"},
				Grain:    "day",
				Range:    &QueryRange{From: "2026-06-26", To: "2026-07-03"},
				Filters:  map[string][]string{"kind": {"verify", "initial"}, "card_entry": {"new"}, "outcome": {"declined"}},
			},
		},
		{
			Intent: "renewals by outcome per month: first-try, recovered, lost and still in dunning",
			Query: Query{
				Measures: []string{"rebills"},
				By:       []string{"time", "outcome"},
				Grain:    "month",
				Range:    &QueryRange{Last: "6m"},
			},
		},
		{
			Intent: "rebill rate and dunning recovery by owner and PSP",
			Query: Query{
				Measures: []string{"rebill_rate", "dunning_recovery_rate"},
				By:       []string{"owner", "psp"},
				Range:    &QueryRange{From: "2026-06-01", To: "2026-06-30"},
			},
		},
		{
			Intent: "dunning recovery curve: cycles collected after a failed first rebill, by attempt",
			Query: Query{
				Measures: []string{"rebills"},
				By:       []string{"owner", "recovery_attempt"},
				Range:    &QueryRange{From: "2026-05-01", To: "2026-06-30"},
				Filters:  map[string][]string{"outcome": {"recovered"}},
			},
		},
		{
			Intent: "missed rebills by why they were missed",
			Query: Query{
				Measures: []string{"rebills"},
				By:       []string{"miss_reason", "psp"},
				Range:    &QueryRange{Last: "30d"},
				Filters:  map[string][]string{"first_outcome": {"missed"}},
			},
		},
		{
			Intent: "NMI's own approvals and declines by month and kind, per PSP, over its history",
			Query: Query{
				Measures: []string{"nmi_history_authorizations"},
				By:       []string{"time", "nmi_kind", "outcome", "psp"},
				Grain:    "month",
				Range:    &QueryRange{Last: "25m"},
			},
		},
		{
			Intent: "net member change per week (plot new, canceled)",
			Query: Query{
				Measures: []string{"new_subscriptions", "cancellations"},
				By:       []string{"time"},
				Grain:    "week",
				Range:    &QueryRange{From: "2026-04-01", To: "2026-06-30"},
			},
		},
		{
			Intent: "current dunning exposure: subscriptions and MRR by status",
			Query: Query{
				Measures: []string{"subscriptions", "mrr"},
				By:       []string{"status"},
				Range:    &QueryRange{From: "2026-07-01", To: "2026-07-03"},
			},
		},
	}
	return doc
}
