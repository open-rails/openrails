package metrics

import "github.com/open-rails/openrails/billing"

// SchemaDoc is the GET /v1/merchant/metrics/schema payload: the registry as
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
		QueryShape: `POST body: {"measures":[...], "by":[dims incl "time"], "grain":"day|week|month|quarter|year", ` +
			`"range":{"from":"YYYY-MM-DD","to":"YYYY-MM-DD"} or {"last":"7d"}, "filters":{dim:[values]}, ` +
			`"order":[{"measure"|"dimension":name,"dir":"asc|desc"}], "limit":N, "compare":"previous_period"}. ` +
			`Dates are inclusive UTC days (RFC3339 accepted, [from,to)); "last" is a relative trailing window ending today ` +
			`(Nd|Nw|Nm|Ny, e.g. "7d" = past 7 days incl. today — prefer it for recurring/saved queries so they stay current). ` +
			`Unknown keys/names are 400s with corrective errors. ` +
			`Response: {columns, rows, grain, range, compare_rows?}; time series zero-fill every bucket.`,
	}
	for i := range Measures {
		m := &Measures[i]
		if m.Internal {
			continue
		}
		doc.Measures = append(doc.Measures, SchemaMeasure{
			Name: m.Name, Class: m.Class, Unit: m.Unit,
			Description: m.Description, Formula: m.Formula,
			Dims: m.Dims, Money: m.Money,
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
			Intent: "KPI row: MRR, net revenue and churn this month vs last month",
			Query: Query{
				Measures: []string{"mrr", "net_revenue", "churn_rate"},
				Range:    &QueryRange{From: "2026-06-01", To: "2026-06-30"},
				Compare:  "previous_period",
			},
		},
		{
			Intent: "weekly net revenue stacked by revenue stream",
			Query: Query{
				Measures: []string{"net_revenue"},
				By:       []string{"time", "stream"},
				Grain:    "week",
				Range:    &QueryRange{From: "2026-05-01", To: "2026-06-30"},
			},
		},
		{
			Intent: "payment health per rail: authorization failure and chargeback rates",
			Query: Query{
				Measures: []string{"attempt_failure_rate", "chargeback_rate"},
				By:       []string{"rail"},
				Range:    &QueryRange{From: "2026-06-01", To: "2026-06-30"},
			},
		},
		{
			Intent: "top 10 customers by usage revenue",
			Query: Query{
				Measures: []string{"usage_revenue"},
				By:       []string{"customer"},
				Range:    &QueryRange{From: "2026-06-01", To: "2026-06-30"},
				Order:    []OrderTerm{{Measure: "usage_revenue", Dir: "desc"}},
				Limit:    intp(10),
			},
		},
		{
			Intent: "new-card decline rate per day and PSP, by decline reason",
			Query: Query{
				Measures: []string{"failed_attempts", "attempt_failure_rate"},
				By:       []string{"time", "psp", "reason"},
				Grain:    "day",
				Range:    &QueryRange{From: "2026-06-26", To: "2026-07-03"},
				Filters:  map[string][]string{"kind": {"verify", "initial"}, "card_entry": {"new"}},
			},
		},
		{
			Intent: "rebill first-attempt failures and missed rebills by owner",
			Query: Query{
				Measures: []string{"rebill_first_failure_rate", "rebill_missed_rate"},
				By:       []string{"owner", "psp"},
				Range:    &QueryRange{From: "2026-06-01", To: "2026-06-30"},
			},
		},
		{
			Intent: "dunning recovery curve: cycles collected after a failed first rebill, by attempt",
			Query: Query{
				Measures: []string{"dunning_recovered"},
				By:       []string{"owner", "recovery_attempt"},
				Range:    &QueryRange{From: "2026-05-01", To: "2026-06-30"},
			},
		},
		{
			Intent: "NMI's own refusal rate by month and kind, per PSP, over its history",
			Query: Query{
				Measures: []string{"nmi_history_authorizations", "nmi_history_refusal_rate"},
				By:       []string{"time", "nmi_kind", "psp"},
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
