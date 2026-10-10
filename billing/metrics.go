package billing

import "time"

// MetricsQuery asks the merchant's metrics: measures, grouped By dimensions
// (and "time" at Grain), over Range, filtered and ordered. GET
// /v1/admin/metrics/schema lists every measure, dimension and grain.
type MetricsQuery struct {
	Measures []string            `json:"measures"`
	By       []string            `json:"by"`
	Grain    string              `json:"grain"`
	Range    *MetricsRange       `json:"range"`
	Filters  map[string][]string `json:"filters"`
	Order    []MetricsOrder      `json:"order"`
	Limit    *int                `json:"limit"`
	// Compare adds the same query over the previous period: "previous_period".
	Compare string `json:"compare"`
}

// MetricsRange bounds a query. Date-only values are UTC calendar days, From
// and To both inclusive; RFC 3339 instants are taken as [From, To). Last is a
// trailing window ending today ("7d", "12w", "6m", "1y"), instead of From/To.
type MetricsRange struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	Last string `json:"last,omitempty"`
}

// MetricsOrder orders rows by a requested measure or dimension; Dir is asc
// (default) or desc.
type MetricsOrder struct {
	Measure   string `json:"measure,omitempty"`
	Dimension string `json:"dimension,omitempty"`
	Dir       string `json:"dir,omitempty"`
}

// MetricsResult is a query's table. A money cell is a decimal string of its
// currency's native units, a count an integer, a ratio a number (null when
// its denominator is zero).
type MetricsResult struct {
	Grain        string              `json:"grain,omitempty"`
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

// MetricsClass is how a measure aggregates: additive, distinct, ratio or
// snapshot.
type MetricsClass string

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

// MetricsMeasure is one measure a query may request.
type MetricsMeasure struct {
	Name        string       `json:"name"`
	Class       MetricsClass `json:"class"`
	Unit        string       `json:"unit"`
	Description string       `json:"description"`
	Formula     string       `json:"formula"`
	Dims        []string     `json:"dims"`
	Money       bool         `json:"money,omitempty"`
}

// MetricsDimension is one axis to group or filter by.
type MetricsDimension struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Values      []string `json:"values,omitempty"`
}

// MetricsExample pairs an intent with the query that answers it.
type MetricsExample struct {
	Intent string       `json:"intent"`
	Query  MetricsQuery `json:"query"`
}

// MetricsLimits are the most buckets and rows a query returns.
type MetricsLimits struct {
	MaxBuckets int `json:"max_buckets"`
	MaxLimit   int `json:"max_limit"`
}

// Dashboard is the merchant's widget layout.
type Dashboard struct {
	Widgets []DashboardWidget `json:"widgets"`
	// IsDefault marks the starting layout of a merchant that saved none.
	IsDefault bool       `json:"is_default"`
	UpdatedAt *time.Time `json:"updated_at"`
	UpdatedBy *string    `json:"updated_by"`
}

// DashboardWidget is one tile: a metrics query, how to draw it, and where.
type DashboardWidget struct {
	ID    string        `json:"id"`
	Title string        `json:"title"`
	Viz   string        `json:"viz"`
	Query MetricsQuery  `json:"query"`
	Grid  DashboardGrid `json:"grid"`
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

// AskMetricsParams is a question about the merchant's metrics.
type AskMetricsParams struct {
	Question string `json:"question"`
}

// MetricsAnswer is a model's answer to a metrics question, with every query
// it ran and its result.
type MetricsAnswer struct {
	Answer   string            `json:"answer"`
	Evidence []MetricsEvidence `json:"evidence"`
}

// MetricsEvidence is one query a metrics answer ran, and its result.
type MetricsEvidence struct {
	Query MetricsQuery `json:"query"`
	MetricsResult
}

// GenerateDashboardWidgetParams describes a widget in words; BaseQuery, when set, is
// an existing widget's query to refine.
type GenerateDashboardWidgetParams struct {
	Prompt    string        `json:"prompt"`
	BaseQuery *MetricsQuery `json:"base_query"`
}

// GeneratedWidget is a validated widget a model wrote from a prompt.
type GeneratedWidget struct {
	Query MetricsQuery `json:"query"`
	Title string       `json:"title"`
	Viz   string       `json:"viz"`
}
