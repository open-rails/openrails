package metrics

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FieldError is one corrective validation error: machine-readable code, the
// offending param, and enough context (did-you-mean + valid list) for a client
// or LLM to self-correct in one retry.
type FieldError struct {
	Code       string   `json:"code"`
	Param      string   `json:"param"`
	Message    string   `json:"message"`
	DidYouMean string   `json:"did_you_mean,omitempty"`
	Valid      []string `json:"valid,omitempty"`
}

// ValidationError carries ALL validation errors at once (never first-error-only).
type ValidationError struct {
	Errors []FieldError `json:"errors"`
}

func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.Errors))
	for i, fe := range e.Errors {
		msgs[i] = fe.Message
	}
	return strings.Join(msgs, "; ")
}

func singleError(fe FieldError) *ValidationError {
	return &ValidationError{Errors: []FieldError{fe}}
}

// Plan is a validated, compilable query.
type Plan struct {
	Measures []*Measure // requested (public) measures, in request order
	Dims     []string   // non-time group-by dims, in output order (incl. implicit currency)
	HasTime  bool
	Grain    string
	From, To time.Time
	Buckets  []time.Time // bucket labels (UTC calendar starts); empty when !HasTime
	Filters  map[string][]string
	Order    []OrderTerm
	Limit    int
	Compare  bool
	// ImplicitCurrency: currency was added as a group-by because a money measure
	// is present without a single-currency filter.
	ImplicitCurrency bool
}

// Validate resolves a query against the registry and returns a compilable plan,
// or every validation error at once. Exported for #741 (dry-run validation).
// Relative ranges resolve against the wall clock; use ValidateAt for a pinned
// clock (tests, deterministic fixtures).
func Validate(q *Query) (*Plan, *ValidationError) {
	return ValidateAt(q, time.Now().UTC())
}

// ValidateAt is Validate with an explicit "now" for relative-range resolution.
func ValidateAt(q *Query, now time.Time) (*Plan, *ValidationError) {
	var errs []FieldError
	add := func(fe FieldError) { errs = append(errs, fe) }

	// --- measures ---------------------------------------------------------------
	if len(q.Measures) == 0 {
		add(FieldError{Code: "missing_measures", Param: "measures",
			Message: "measures is required: pick at least one measure", Valid: PublicMeasureNames()})
	}
	var measures []*Measure
	seenMeasure := map[string]bool{}
	for i, name := range q.Measures {
		m, ok := measureByName[name]
		if !ok || m.Internal {
			fe := FieldError{Code: "unknown_measure", Param: fmt.Sprintf("measures[%d]", i),
				Message: fmt.Sprintf("unknown measure %q", name), Valid: PublicMeasureNames()}
			if s, ok := suggest(name, fe.Valid); ok {
				fe.DidYouMean = s
				fe.Message = fmt.Sprintf("unknown measure %q: did you mean %q?", name, s)
			}
			add(fe)
			continue
		}
		if seenMeasure[name] {
			add(FieldError{Code: "duplicate_measure", Param: fmt.Sprintf("measures[%d]", i),
				Message: fmt.Sprintf("measure %q requested more than once", name)})
			continue
		}
		seenMeasure[name] = true
		measures = append(measures, m)
	}

	// --- by (dimensions) ----------------------------------------------------------
	hasTime := false
	var dims []string
	seenDim := map[string]bool{}
	for i, name := range q.By {
		if name == "time" {
			if hasTime {
				add(FieldError{Code: "duplicate_dimension", Param: fmt.Sprintf("by[%d]", i), Message: `"time" requested more than once`})
			}
			hasTime = true
			continue
		}
		d, ok := dimensionByName[name]
		if !ok {
			fe := FieldError{Code: "unknown_dimension", Param: fmt.Sprintf("by[%d]", i),
				Message: fmt.Sprintf("unknown dimension %q", name), Valid: DimensionNames()}
			if s, ok := suggest(name, fe.Valid); ok {
				fe.DidYouMean = s
				fe.Message = fmt.Sprintf("unknown dimension %q: did you mean %q?", name, s)
			}
			add(fe)
			continue
		}
		if seenDim[d.Name] {
			add(FieldError{Code: "duplicate_dimension", Param: fmt.Sprintf("by[%d]", i),
				Message: fmt.Sprintf("dimension %q requested more than once", name)})
			continue
		}
		seenDim[d.Name] = true
		dims = append(dims, d.Name)
	}
	// Per-measure allowed dims.
	for _, m := range measures {
		for _, dim := range dims {
			if !m.allowsDim(dim) {
				add(FieldError{Code: "dimension_not_allowed", Param: "by",
					Message: fmt.Sprintf("measure %q does not support dimension %q; supported: %s (plus time)", m.Name, dim, strings.Join(m.Dims, ", ")),
					Valid:   m.Dims})
			}
		}
	}

	// --- grain -----------------------------------------------------------------
	grain := q.Grain
	if grain == "" {
		grain = "day"
	}
	validGrain := false
	for _, g := range Grains {
		if grain == g {
			validGrain = true
		}
	}
	if !validGrain {
		fe := FieldError{Code: "invalid_grain", Param: "grain",
			Message: fmt.Sprintf("unknown grain %q", q.Grain), Valid: Grains}
		if s, ok := suggest(q.Grain, Grains); ok {
			fe.DidYouMean = s
			fe.Message = fmt.Sprintf("unknown grain %q: did you mean %q?", q.Grain, s)
		}
		add(fe)
	}

	// --- range -------------------------------------------------------------------
	var from, to time.Time
	if q.Range == nil || (q.Range.Last == "" && (q.Range.From == "" || q.Range.To == "")) {
		add(FieldError{Code: "missing_range", Param: "range",
			Message: `range is required: {"from":"YYYY-MM-DD","to":"YYYY-MM-DD"} (dates are inclusive UTC days), RFC3339 timestamps [from,to), or a relative window {"last":"7d"} (Nd|Nw|Nm|Ny, ending today)`})
	} else if q.Range.Last != "" {
		if q.Range.From != "" || q.Range.To != "" {
			add(FieldError{Code: "invalid_range", Param: "range",
				Message: `range.last is mutually exclusive with range.from/range.to: use either a relative window or explicit dates`})
		} else {
			var ok bool
			from, to, ok = resolveLast(q.Range.Last, now)
			if !ok {
				add(FieldError{Code: "invalid_range", Param: "range.last",
					Message: fmt.Sprintf(`could not parse %q: use "<N>d", "<N>w", "<N>m" or "<N>y" (e.g. "7d" = the past 7 days incl. today)`, q.Range.Last)})
			}
		}
	} else {
		var okFrom, okTo bool
		from, okFrom = parseRangeTime(q.Range.From, false)
		to, okTo = parseRangeTime(q.Range.To, true)
		if !okFrom {
			add(FieldError{Code: "invalid_range", Param: "range.from",
				Message: fmt.Sprintf("could not parse %q: use YYYY-MM-DD or RFC3339", q.Range.From)})
		}
		if !okTo {
			add(FieldError{Code: "invalid_range", Param: "range.to",
				Message: fmt.Sprintf("could not parse %q: use YYYY-MM-DD or RFC3339", q.Range.To)})
		}
		if okFrom && okTo && !from.Before(to) {
			add(FieldError{Code: "invalid_range", Param: "range",
				Message: fmt.Sprintf("range.from (%s) must be before range.to (%s)", q.Range.From, q.Range.To)})
		}
	}

	// --- buckets (clamp) ------------------------------------------------------------
	var buckets []time.Time
	if hasTime && validGrain && !from.IsZero() && from.Before(to) {
		buckets = bucketLabels(from, to, grain)
		if len(buckets) > MaxBuckets {
			add(FieldError{Code: "range_too_many_buckets", Param: "range",
				Message: fmt.Sprintf("range at grain %q yields %d buckets; max %d — shrink the range or coarsen the grain", grain, len(buckets), MaxBuckets)})
		}
	}

	// --- filters --------------------------------------------------------------------
	filters := map[string][]string{}
	filterDims := make([]string, 0, len(q.Filters))
	for name := range q.Filters {
		filterDims = append(filterDims, name)
	}
	sort.Strings(filterDims)
	for _, name := range filterDims {
		vals := q.Filters[name]
		d, ok := dimensionByName[name]
		if !ok || name == "time" {
			fe := FieldError{Code: "unknown_filter_dimension", Param: "filters." + name,
				Message: fmt.Sprintf("unknown filter dimension %q (time is filtered via range)", name), Valid: filterableDimNames()}
			if s, ok := suggest(name, fe.Valid); ok {
				fe.DidYouMean = s
				fe.Message = fmt.Sprintf("unknown filter dimension %q: did you mean %q?", name, s)
			}
			add(fe)
			continue
		}
		if len(vals) == 0 {
			add(FieldError{Code: "invalid_filter_value", Param: "filters." + name,
				Message: fmt.Sprintf("filter %q needs at least one value", name)})
			continue
		}
		if d.Values != nil {
			for _, v := range vals {
				if !contains(d.Values, v) {
					fe := FieldError{Code: "invalid_filter_value", Param: "filters." + name,
						Message: fmt.Sprintf("invalid value %q for %q", v, name), Valid: d.Values}
					if s, ok := suggest(v, d.Values); ok {
						fe.DidYouMean = s
						fe.Message = fmt.Sprintf("invalid value %q for %q: did you mean %q?", v, name, s)
					}
					add(fe)
				}
			}
		}
		// Filters must be honored by every requested measure (a silently ignored
		// filter is worse than a 400).
		for _, m := range measures {
			if !m.allowsDim(name) {
				add(FieldError{Code: "dimension_not_allowed", Param: "filters." + name,
					Message: fmt.Sprintf("measure %q cannot be filtered by %q; supported: %s", m.Name, name, strings.Join(m.Dims, ", ")),
					Valid:   m.Dims})
			}
		}
		filters[name] = vals
	}

	// --- order ---------------------------------------------------------------------
	for i, o := range q.Order {
		if (o.Measure == "") == (o.Dimension == "") {
			add(FieldError{Code: "invalid_order", Param: fmt.Sprintf("order[%d]", i),
				Message: `each order term needs exactly one of "measure" or "dimension"`})
			continue
		}
		if o.Dir != "" && o.Dir != "asc" && o.Dir != "desc" {
			add(FieldError{Code: "invalid_order", Param: fmt.Sprintf("order[%d].dir", i),
				Message: fmt.Sprintf("invalid dir %q", o.Dir), Valid: []string{"asc", "desc"}})
		}
		if o.Measure != "" && !seenMeasure[o.Measure] {
			add(FieldError{Code: "invalid_order", Param: fmt.Sprintf("order[%d].measure", i),
				Message: fmt.Sprintf("order measure %q is not in measures", o.Measure), Valid: q.Measures})
		}
		if o.Dimension != "" && o.Dimension != "time" && !seenDim[o.Dimension] {
			add(FieldError{Code: "invalid_order", Param: fmt.Sprintf("order[%d].dimension", i),
				Message: fmt.Sprintf("order dimension %q is not in by", o.Dimension), Valid: q.By})
		}
	}

	// --- limit ------------------------------------------------------------------------
	limit := DefaultLimit
	if q.Limit != nil {
		limit = *q.Limit
		if limit < 1 || limit > MaxLimit {
			add(FieldError{Code: "invalid_limit", Param: "limit",
				Message: fmt.Sprintf("limit %d out of range [1, %d]", limit, MaxLimit)})
		}
	}

	// --- compare ------------------------------------------------------------------------
	compare := false
	switch q.Compare {
	case "":
	case "previous_period":
		compare = true
	default:
		fe := FieldError{Code: "invalid_compare", Param: "compare",
			Message: fmt.Sprintf("unknown compare %q", q.Compare), Valid: []string{"previous_period"}}
		if s, ok := suggest(q.Compare, fe.Valid); ok {
			fe.DidYouMean = s
			fe.Message = fmt.Sprintf("unknown compare %q: did you mean %q?", q.Compare, s)
		}
		add(fe)
	}

	// --- implicit currency ---------------------------------------------------------------
	implicitCurrency := false
	moneyPresent := false
	for _, m := range measures {
		if m.Money {
			moneyPresent = true
		}
	}
	if moneyPresent && !seenDim["currency"] && len(filters["currency"]) != 1 {
		implicitCurrency = true
		dims = append(dims, "currency")
		// Every money measure supports currency by construction; non-money
		// measures in the same query must tolerate the implicit grouping too.
		for _, m := range measures {
			if !m.allowsDim("currency") {
				add(FieldError{Code: "dimension_not_allowed", Param: "measures",
					Message: fmt.Sprintf("query mixes money measures (implicit currency group-by) with %q, which does not support the currency dimension; filter to one currency or drop one side", m.Name),
					Valid:   m.Dims})
			}
		}
	}

	if len(errs) > 0 {
		return nil, &ValidationError{Errors: errs}
	}
	return &Plan{
		Measures: measures, Dims: dims, HasTime: hasTime, Grain: grain,
		From: from, To: to, Buckets: buckets, Filters: filters,
		Order: q.Order, Limit: limit, Compare: compare,
		ImplicitCurrency: implicitCurrency,
	}, nil
}

var lastRangeRe = regexp.MustCompile(`^([0-9]+)([dwmy])$`)

// resolveLast turns a relative window ("7d", "12w", "6m", "1y") into an
// inclusive UTC day range ending today: "7d" = the past 7 calendar days
// including today. Returned bounds match date-form from/to semantics
// (from = day start, to = day end exclusive).
func resolveLast(last string, now time.Time) (from, to time.Time, ok bool) {
	m := lastRangeRe.FindStringSubmatch(last)
	if m == nil {
		return time.Time{}, time.Time{}, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return time.Time{}, time.Time{}, false
	}
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	to = today.AddDate(0, 0, 1) // inclusive of today
	switch m[2] {
	case "d":
		from = to.AddDate(0, 0, -n)
	case "w":
		from = to.AddDate(0, 0, -7*n)
	case "m":
		from = to.AddDate(0, -n, 0)
	case "y":
		from = to.AddDate(-n, 0, 0)
	}
	return from, to, true
}

func filterableDimNames() []string {
	out := make([]string, 0, len(Dimensions))
	for i := range Dimensions {
		if Dimensions[i].Name != "time" {
			out = append(out, Dimensions[i].Name)
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// bucketLabels enumerates UTC calendar bucket starts covering [from, to).
// Labels match Postgres date_trunc (weeks start Monday).
func bucketLabels(from, to time.Time, grain string) []time.Time {
	cur := truncGrain(from, grain)
	var out []time.Time
	for cur.Before(to) {
		out = append(out, cur)
		cur = stepGrain(cur, grain)
		if len(out) > MaxBuckets {
			break // clamp check reports the error; avoid unbounded loops
		}
	}
	return out
}

func truncGrain(t time.Time, grain string) time.Time {
	t = t.UTC()
	switch grain {
	case "day":
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	case "week":
		d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		offset := (int(d.Weekday()) + 6) % 7 // Monday start, like date_trunc
		return d.AddDate(0, 0, -offset)
	case "month":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	case "quarter":
		qm := time.Month(((int(t.Month())-1)/3)*3 + 1)
		return time.Date(t.Year(), qm, 1, 0, 0, 0, 0, time.UTC)
	case "year":
		return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return t
}

func stepGrain(t time.Time, grain string) time.Time {
	switch grain {
	case "day":
		return t.AddDate(0, 0, 1)
	case "week":
		return t.AddDate(0, 0, 7)
	case "month":
		return t.AddDate(0, 1, 0)
	case "quarter":
		return t.AddDate(0, 3, 0)
	case "year":
		return t.AddDate(1, 0, 0)
	}
	return t
}
