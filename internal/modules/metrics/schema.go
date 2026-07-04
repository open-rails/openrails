package metrics

// SchemaDoc is the GET /v1/merchant/metrics/schema payload: the registry as
// the LLM-legible context document. One source of truth — the same registry
// drives enforcement, so this cannot drift.
type SchemaDoc struct {
	Measures   []SchemaMeasure   `json:"measures"`
	Dimensions []SchemaDimension `json:"dimensions"`
	Grains     []string          `json:"grains"`
	Derived    []DerivedFormula  `json:"derived"`
	Deferred   []string          `json:"deferred"`
	Caveats    []string          `json:"caveats"`
	Examples   []SchemaExample   `json:"examples"`
	Limits     SchemaLimits      `json:"limits"`
	QueryShape string            `json:"query_shape"`
}

// SchemaMeasure is one requestable measure.
type SchemaMeasure struct {
	Name        string   `json:"name"`
	Class       Class    `json:"class"`
	Unit        string   `json:"unit"`
	Description string   `json:"description"`
	Formula     string   `json:"formula"`
	Dims        []string `json:"dims"`
	Money       bool     `json:"money,omitempty"`
}

// SchemaDimension is one group-by/filter axis.
type SchemaDimension struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Values      []string `json:"values,omitempty"`
}

// SchemaExample pairs a natural-language intent with the query JSON that
// answers it.
type SchemaExample struct {
	Intent string `json:"intent"`
	Query  Query  `json:"query"`
}

// SchemaLimits are the engine clamps.
type SchemaLimits struct {
	MaxBuckets int `json:"max_buckets"`
	MaxLimit   int `json:"max_limit"`
}

func intp(v int) *int { return &v }

// Schema builds the registry dump.
func Schema() SchemaDoc {
	doc := SchemaDoc{
		Grains:   Grains,
		Derived:  Derived,
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
			Intent: "count of users who cancelled per day, for the past 7 days",
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
			Intent: "payment health per rail: approval and chargeback rates",
			Query: Query{
				Measures: []string{"approval_rate", "chargeback_rate"},
				By:       []string{"rail"},
				Range:    &QueryRange{From: "2026-06-01", To: "2026-06-30"},
			},
		},
		{
			Intent: "top 10 payers by usage revenue",
			Query: Query{
				Measures: []string{"usage_revenue"},
				By:       []string{"payer"},
				Range:    &QueryRange{From: "2026-06-01", To: "2026-06-30"},
				Order:    []OrderTerm{{Measure: "usage_revenue", Dir: "desc"}},
				Limit:    intp(10),
			},
		},
		{
			Intent: "how many distinct users had a failed FIRST payment each day, by decline reason",
			Query: Query{
				Measures: []string{"unique_failed_customers"},
				By:       []string{"time", "failure_reason"},
				Grain:    "day",
				Range:    &QueryRange{From: "2026-06-26", To: "2026-07-03"},
				Filters:  map[string][]string{"attempt_kind": {"initial"}},
			},
		},
		{
			Intent: "net member change per week (plot new, cancelled)",
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
