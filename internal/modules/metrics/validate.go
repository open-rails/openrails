package metrics

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/calendarutil"
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
	// Zone is the merchant's accounting timezone: its midnights start days,
	// weeks, months, quarters and years.
	Zone    *time.Location
	Buckets []time.Time // bucket labels (calendar starts in Zone, as UTC instants); empty when !HasTime
	Filters map[string][]string
	Order   []OrderTerm
	Limit   int
	// Compare is the comparison window: "", previous_period or previous_year.
	Compare string
	// ImplicitCurrency: currency was added as a group-by because a money measure
	// is present without a single-currency filter.
	ImplicitCurrency bool
}

// zone is the plan's calendar, UTC when none was named.
func (p *Plan) zone() *time.Location {
	if p.Zone == nil {
		return time.UTC
	}
	return p.Zone
}

// ValidateAt is ValidateIn in UTC.
func ValidateAt(q *Query, now time.Time) (*Plan, *ValidationError) {
	return ValidateIn(q, now, time.UTC)
}

// ValidateIn validates q with an explicit "now" for relative ranges, its
// calendar days in zone.
func ValidateIn(q *Query, now time.Time, zone *time.Location) (*Plan, *ValidationError) {
	var errs []FieldError
	add := func(fe FieldError) { errs = append(errs, fe) }

	if len(q.Measures) == 0 {
		add(FieldError{Code: "missing_measures", Param: "measures",
			Message: "measures is required: pick at least one measure", Valid: PublicMeasureNames()})
	}
	var measures []*Measure
	seenMeasure := map[string]bool{}
	for i, name := range q.Measures {
		m, ok := measureByName[name]
		if !ok || m.Internal {
			add(unknownMeasure(fmt.Sprintf("measures[%d]", i), name))
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
			add(unknownDimension("unknown_dimension", fmt.Sprintf("by[%d]", i), name, DimensionNames()))
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

	grain := q.Grain
	if grain == "" {
		grain = "day"
	}
	validGrain := contains(Grains, grain)
	if !validGrain {
		fe := FieldError{Code: "invalid_grain", Param: "grain",
			Message: fmt.Sprintf("unknown grain %q", q.Grain), Valid: Grains}
		if s, ok := suggest(q.Grain, Grains); ok {
			fe.DidYouMean = s
			fe.Message = fmt.Sprintf("unknown grain %q: did you mean %q?", q.Grain, s)
		}
		add(fe)
	}

	var from, to time.Time
	if q.Range == nil || (q.Range.Last == "" && (q.Range.From == "" || q.Range.To == "")) {
		add(FieldError{Code: "missing_range", Param: "range",
			Message: `range is required: {"from":"YYYY-MM-DD","to":"YYYY-MM-DD"} (dates are inclusive days in the accounting timezone), RFC3339 timestamps [from,to), or a relative window {"last":"7d"} (Nd|Nw|Nm|Ny, ending today)`})
	} else if q.Range.Last != "" {
		if q.Range.From != "" || q.Range.To != "" {
			add(FieldError{Code: "invalid_range", Param: "range",
				Message: `range.last is mutually exclusive with range.from/range.to: use either a relative window or explicit dates`})
		} else {
			var ok bool
			from, to, ok = resolveLast(q.Range.Last, now, zone)
			if !ok {
				add(FieldError{Code: "invalid_range", Param: "range.last",
					Message: fmt.Sprintf(`could not parse %q: use "<N>d", "<N>w", "<N>m" or "<N>y" (e.g. "7d" = the past 7 days incl. today)`, q.Range.Last)})
			}
		}
	} else {
		var okFrom, okTo bool
		from, okFrom = parseRangeTime(q.Range.From, false, zone)
		to, okTo = parseRangeTime(q.Range.To, true, zone)
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

	if grain == "auto" && !from.IsZero() && from.Before(to) {
		grain = autoGrain(from, to)
	}
	var buckets []time.Time
	if hasTime && validGrain && grain != "auto" && !from.IsZero() && from.Before(to) {
		buckets = bucketLabels(from, to, grain, zone)
		if len(buckets) > MaxBuckets {
			add(FieldError{Code: "range_too_many_buckets", Param: "range",
				Message: fmt.Sprintf("range at grain %q yields %d buckets; max %d — shrink the range or coarsen the grain", grain, len(buckets), MaxBuckets)})
		}
	}

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
			add(unknownDimension("unknown_filter_dimension", "filters."+name, name, filterableDimNames()))
			continue
		}
		if len(vals) == 0 {
			add(FieldError{Code: "invalid_filter_value", Param: "filters." + name,
				Message: fmt.Sprintf("filter %q needs at least one value", name)})
			continue
		}
		// A dimension's vocabulary may depend on the measure (outcome).
		refused := map[string]bool{}
		for _, m := range measures {
			valid := m.valuesOf(name)
			if valid == nil {
				continue
			}
			for _, v := range vals {
				if contains(valid, v) || refused[v] {
					continue
				}
				refused[v] = true
				fe := FieldError{Code: "invalid_filter_value", Param: "filters." + name,
					Message: fmt.Sprintf("invalid value %q for %q on %q", v, name, m.Name), Valid: valid}
				if s, ok := suggest(v, valid); ok {
					fe.DidYouMean = s
					fe.Message = fmt.Sprintf("invalid value %q for %q on %q: did you mean %q?", v, name, m.Name, s)
				}
				add(fe)
			}
		}
		if d.Parse != nil {
			parsed := make([]string, 0, len(vals))
			for _, v := range vals {
				canonical, err := d.Parse(v)
				if err != nil {
					add(FieldError{Code: "invalid_filter_value", Param: "filters." + name,
						Message: fmt.Sprintf("invalid value %q for %q: %v", v, name, err)})
					continue
				}
				parsed = append(parsed, canonical)
			}
			vals = parsed
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
		if name == "currency" {
			// Stored currency is upper case, so "usd" must still match;
			// normalizing here keeps the SQL comparison index-friendly.
			normalized := make([]string, 0, len(vals))
			for _, v := range vals {
				normalized = append(normalized, money.NormalizeCurrency(v))
			}
			vals = normalized
		}
		filters[name] = vals
	}

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

	limit := DefaultLimit
	if q.Limit != nil {
		limit = *q.Limit
		if limit < 1 || limit > MaxLimit {
			add(FieldError{Code: "invalid_limit", Param: "limit",
				Message: fmt.Sprintf("limit %d out of range [1, %d]", limit, MaxLimit)})
		}
	}

	if q.Compare != "" && !contains(Compares, q.Compare) {
		fe := FieldError{Code: "invalid_compare", Param: "compare",
			Message: fmt.Sprintf("unknown compare %q", q.Compare), Valid: Compares}
		if s, ok := suggest(q.Compare, fe.Valid); ok {
			fe.DidYouMean = s
			fe.Message = fmt.Sprintf("unknown compare %q: did you mean %q?", q.Compare, s)
		}
		add(fe)
	}

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
		From: from, To: to, Zone: zone, Buckets: buckets, Filters: filters,
		Order: q.Order, Limit: limit, Compare: q.Compare,
		ImplicitCurrency: implicitCurrency,
	}, nil
}

var lastRangeRe = regexp.MustCompile(`^([0-9]+)([dwmy])$`)

// resolveLast turns a relative window ("7d", "12w", "6m", "1y") into an
// inclusive day range in zone ending today: "7d" = the past 7 calendar days
// including today. Returned bounds match date-form from/to semantics
// (from = day start, to = day end exclusive), as UTC instants.
func resolveLast(last string, now time.Time, zone *time.Location) (from, to time.Time, ok bool) {
	m := lastRangeRe.FindStringSubmatch(last)
	if m == nil {
		return time.Time{}, time.Time{}, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return time.Time{}, time.Time{}, false
	}
	// Dates, not wall clocks: a day that starts at a transition stays whole.
	y, mo, d := now.In(zone).Date()
	d++ // inclusive of today
	to = calendarutil.DayStart(y, mo, d, zone)
	switch m[2] {
	case "d":
		from = calendarutil.DayStart(y, mo, d-n, zone)
	case "w":
		from = calendarutil.DayStart(y, mo, d-7*n, zone)
	case "m":
		from = calendarutil.DayStart(y, mo-time.Month(n), d, zone)
	case "y":
		from = calendarutil.DayStart(y-n, mo, d, zone)
	}
	return from.UTC(), to.UTC(), true
}

// autoGrain is the grain a range reads best at: days up to 45, weeks up to
// 26, then months.
func autoGrain(from, to time.Time) string {
	switch days := to.Sub(from).Hours() / 24; {
	case days <= 45:
		return "day"
	case days <= 26*7:
		return "week"
	}
	return "month"
}

// unknownMeasure refuses a name the registry does not serve: a removed one
// names its replacement, a planned one says so, any other gets a
// did-you-mean.
func unknownMeasure(param, name string) FieldError {
	fe := FieldError{Code: "unknown_measure", Param: param, Valid: PublicMeasureNames()}
	if r, ok := Removed[name]; ok {
		fe.DidYouMean = r.Measure
		fe.Message = fmt.Sprintf("measure %q was removed: use %q, %s", name, r.Measure, r.How)
		if r.Measure == "" {
			fe.Message = fmt.Sprintf("measure %q was removed: %s", name, r.How)
		}
		return fe
	}
	if contains(Deferred, name) {
		fe.Message = fmt.Sprintf("measure %q is planned and not available yet", name)
		return fe
	}
	fe.Message = fmt.Sprintf("unknown measure %q", name)
	if s, ok := suggest(name, fe.Valid); ok {
		fe.DidYouMean = s
		fe.Message = fmt.Sprintf("unknown measure %q: did you mean %q?", name, s)
	}
	return fe
}

func unknownDimension(code, param, name string, valid []string) FieldError {
	fe := FieldError{Code: code, Param: param, Valid: valid}
	if r, ok := RemovedDimensions[name]; ok {
		fe.DidYouMean = r
		fe.Message = fmt.Sprintf("dimension %q was renamed %q", name, r)
		return fe
	}
	fe.Message = fmt.Sprintf("unknown dimension %q", name)
	if code == "unknown_filter_dimension" {
		fe.Message = fmt.Sprintf("unknown filter dimension %q (time is filtered via range)", name)
	}
	if s, ok := suggest(name, valid); ok {
		fe.DidYouMean = s
		fe.Message += fmt.Sprintf(": did you mean %q?", s)
	}
	return fe
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

// bucketLabels enumerates the calendar bucket starts in zone covering
// [from, to), as UTC instants: each its date's first instant, which is what
// PostgreSQL's date_trunc(grain, t, zone) gives (weeks start Monday). Each
// is computed from its date, never by moving a wall clock, so a day that
// starts at a transition neither drifts nor repeats.
func bucketLabels(from, to time.Time, grain string, zone *time.Location) []time.Time {
	y, m, d := bucketDate(from.In(zone), grain)
	var out []time.Time
	for cur := calendarutil.DayStart(y, m, d, zone); cur.Before(to); cur = calendarutil.DayStart(y, m, d, zone) {
		out = append(out, cur.UTC())
		y, m, d = nextBucketDate(y, m, d, grain)
		if len(out) > MaxBuckets {
			break // clamp check reports the error; avoid unbounded loops
		}
	}
	return out
}

// truncGrain is the start of t's bucket on zone's calendar.
func truncGrain(t time.Time, grain string, zone *time.Location) time.Time {
	y, m, d := bucketDate(t.In(zone), grain)
	return calendarutil.DayStart(y, m, d, zone).UTC()
}

// bucketDate is the date t's bucket starts on, in t's location.
func bucketDate(t time.Time, grain string) (int, time.Month, int) {
	y, m, d := t.Date()
	switch grain {
	case "week":
		d -= (int(t.Weekday()) + 6) % 7 // Monday start, like date_trunc
	case "month":
		d = 1
	case "quarter":
		m, d = ((m-1)/3)*3+1, 1
	case "year":
		m, d = 1, 1
	}
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Date()
}

// nextBucketDate is the date the bucket after the one starting on y-m-d
// starts on.
func nextBucketDate(y int, m time.Month, d int, grain string) (int, time.Month, int) {
	switch grain {
	case "day":
		d++
	case "week":
		d += 7
	case "month":
		m++
	case "quarter":
		m += 3
	case "year":
		y++
	}
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Date()
}

// ReadsUsage reports a plan whose measures, or their components, read usage
// (the usage and usage_charged families): the event store's.
func (p *Plan) ReadsUsage() bool {
	var reads func(m *Measure, depth int) bool
	reads = func(m *Measure, depth int) bool {
		if m == nil || depth > 8 {
			return false
		}
		if m.Family == FamUsage || m.Family == FamUsageCharged {
			return true
		}
		return reads(measureByName[m.Num], depth+1) || reads(measureByName[m.Den], depth+1)
	}
	for _, m := range p.Measures {
		if reads(m, 0) {
			return true
		}
	}
	return false
}
