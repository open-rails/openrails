package billing

import "time"

// QueryMetricsParams asks the merchant's metrics: measures, grouped By dimensions
// (and "time" at Grain; "auto" picks day, week or month from the range), over
// Range, filtered and ordered. GET /v1/metrics/schema lists every measure,
// dimension and grain.
type QueryMetricsParams struct {
	Measures []string            `json:"measures"`
	By       []string            `json:"by"`
	Grain    string              `json:"grain"`
	Range    *MetricsRange       `json:"range"`
	Filters  map[string][]string `json:"filters"`
	Order    []MetricsOrder      `json:"order"`
	Limit    *int                `json:"limit"`
	// Compare adds the same query over the window before ("previous_period")
	// or a year before ("previous_year").
	Compare string `json:"compare"`
}

// MetricsRange bounds a query. Date-only values are calendar days in the
// merchant's accounting timezone, From and To both inclusive; RFC 3339
// instants are taken as [From, To). Last is a trailing window ending today
// there ("7d", "12w", "6m", "1y"), and Next one starting today (what is
// scheduled ahead), instead of From/To.
type MetricsRange struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	Last string `json:"last,omitempty"`
	Next string `json:"next,omitempty"`
}

// MetricsOrder invoices rows by a requested measure or dimension; Dir is asc
// (default) or desc.
type MetricsOrder struct {
	Measure   string `json:"measure,omitempty"`
	Dimension string `json:"dimension,omitempty"`
	Dir       string `json:"dir,omitempty"`
}

// MetricsResult is a query's table. A money cell is a decimal string of its
// currency's native units, a count an integer, a ratio a number (null when
// its denominator is zero). A time cell is the instant its bucket starts:
// days, weeks, months, quarters and years begin at midnight in Timezone, the
// merchant's accounting timezone.
type MetricsResult struct {
	Grain        string              `json:"grain,omitempty"`
	Timezone     string              `json:"timezone"`
	Range        MetricsResultRange  `json:"range"`
	Columns      []MetricsColumn     `json:"columns"`
	Rows         [][]any             `json:"rows"`
	CompareRange *MetricsResultRange `json:"compare_range,omitempty"`
	CompareRows  [][]any             `json:"compare_rows,omitempty"`
}

// MetricsResultRange is the [From, To) a result covers.
type MetricsResultRange struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// MetricsColumn names a result column: Kind is time, dimension or measure.
type MetricsColumn struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Unit string `json:"unit,omitempty"`
}

// MetricsClass is how a measure aggregates: additive, distinct, ratio,
// difference or snapshot.
type MetricsClass string

// MetricsGroup is the view a measure belongs to: business (the health
// overview) or ops (operator signals).
type MetricsGroup string

// MetricsDirection is which way a change of a measure is good news:
// up_is_good, down_is_good or neutral.
type MetricsDirection string

// MetricsThreshold is a level drawn across a measure's chart: Value in the
// measure's unit (a ratio as a fraction), Label naming it. From and Until
// bound the months it is in force (Until excluded); nil is open.
type MetricsThreshold struct {
	Value float64    `json:"value"`
	Label string     `json:"label"`
	From  *time.Time `json:"from,omitempty"`
	Until *time.Time `json:"until,omitempty"`
}

// MetricsSchema is every measure, dimension and grain a query may use, with
// examples: the document a client or a model writes queries from.
type MetricsSchema struct {
	Measures   []MetricsMeasure   `json:"measures"`
	Dimensions []MetricsDimension `json:"dimensions"`
	Grains     []string           `json:"grains"`
	Deferred   []string           `json:"deferred"`
	Caveats    []string           `json:"caveats"`
	Examples   []MetricsExample   `json:"examples"`
	Limits     MetricsLimits      `json:"limits"`
	QueryShape string             `json:"query_shape"`
}

// MetricsMeasure is one measure a query may request. DimValues lists the
// values a dimension takes on this measure where they differ from the
// dimension's own list (outcome, kind).
type MetricsMeasure struct {
	Name        string              `json:"name"`
	Class       MetricsClass        `json:"class"`
	Unit        string              `json:"unit"`
	Group       MetricsGroup        `json:"group"`
	Direction   MetricsDirection    `json:"direction"`
	Thresholds  []MetricsThreshold  `json:"thresholds"`
	Description string              `json:"description"`
	Formula     string              `json:"formula"`
	Dims        []string            `json:"dims"`
	DimValues   map[string][]string `json:"dim_values,omitempty"`
	Money       bool                `json:"money,omitempty"`
	// UTCDays: the measure reads a rollup kept per UTC date, so its days are
	// UTC dates, grouped as the accounting timezone's days of the same date.
	UTCDays bool `json:"utc_days,omitempty"`
	// Program is the dispute monitoring program whose ratio this is: its
	// standing (ListDisputePrograms) says which level is reached, minimum
	// counts applied, which the ratio alone does not.
	Program DisputeProgram `json:"program,omitempty"`
}

// MetricsDimension is one axis to group or filter by.
type MetricsDimension struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Values      []string `json:"values,omitempty"`
}

// MetricsExample pairs an intent with the query that answers it.
type MetricsExample struct {
	Intent string             `json:"intent"`
	Query  QueryMetricsParams `json:"query"`
}

// MetricsLimits are the most buckets and rows a query returns.
type MetricsLimits struct {
	MaxBuckets int `json:"max_buckets"`
	MaxLimit   int `json:"max_limit"`
}

// Dashboard is the merchant's widget layout.
type Dashboard struct {
	Widgets []DashboardWidget `json:"widgets"`
	// IsDefault marks the starting layout of a merchant that saved none;
	// Profile is what that layout was built for, nil on a saved one.
	IsDefault bool              `json:"is_default"`
	Profile   *DashboardProfile `json:"profile"`
	UpdatedAt *time.Time        `json:"updated_at"`
	UpdatedBy *string           `json:"updated_by"`
}

// DashboardProfile is what the merchant's data shows it sells: subscriptions,
// card payments, metered usage and prepaid credits. All false is a merchant
// with no activity yet.
type DashboardProfile struct {
	Subscriptions bool `json:"subscriptions"`
	CardPayments  bool `json:"card_payments"`
	Usage         bool `json:"usage"`
	Credits       bool `json:"credits"`
	// Checkouts: the merchant's customers pay checkouts OpenRails records.
	Checkouts bool `json:"checkouts"`
}

// DashboardWidget is one tile: a metrics query, how to draw it, and where.
// A dashboard shows every widget over the range its viewer picks; a
// FixedRange widget keeps its query's own range. A heading widget is a
// section title with an empty query.
type DashboardWidget struct {
	ID         string             `json:"id"`
	Title      string             `json:"title"`
	Viz        string             `json:"viz"`
	Query      QueryMetricsParams `json:"query"`
	Grid       DashboardGrid      `json:"grid"`
	FixedRange bool               `json:"fixed_range"`
}

// DashboardGrid places a widget on the dashboard's grid.
type DashboardGrid struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// SetDashboardParams replaces the merchant's layout.
type SetDashboardParams struct {
	Widgets []DashboardWidget `json:"widgets"`
}
