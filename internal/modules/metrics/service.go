package metrics

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Service executes validated metrics plans. RLS pinning is structural: every
// statement runs inside MerchantTx.
type Service struct {
	db *db.DB
}

// NewService binds the metrics engine to the database.
func NewService(database *db.DB) *Service {
	return &Service{db: database}
}

// Column describes one result column.
type Column struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // time | dimension | measure
	Unit string `json:"unit,omitempty"`
}

// RangeOut echoes the resolved query window.
type RangeOut struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Result is the tabular query response: token-lean columns + rows.
type Result struct {
	Grain        string    `json:"grain,omitempty"`
	Range        RangeOut  `json:"range"`
	Columns      []Column  `json:"columns"`
	Rows         [][]any   `json:"rows"`
	CompareRange *RangeOut `json:"compare_range,omitempty"`
	CompareRows  [][]any   `json:"compare_rows,omitempty"`
}

// Execute runs a validated plan and returns the merged tabular result.
func (s *Service) Execute(ctx context.Context, plan *Plan) (*Result, error) {
	rows, err := s.run(ctx, plan)
	if err != nil {
		return nil, err
	}
	res := &Result{
		Range:   RangeOut{From: plan.From, To: plan.To},
		Columns: columns(plan),
		Rows:    rows,
	}
	if plan.HasTime {
		res.Grain = plan.Grain
	}
	if plan.Compare {
		shift := plan.To.Sub(plan.From)
		prev := *plan
		prev.From, prev.To = plan.From.Add(-shift), plan.From
		prev.Compare = false
		if prev.HasTime {
			prev.Buckets = bucketLabels(prev.From, prev.To, prev.Grain)
		}
		prevRows, err := s.run(ctx, &prev)
		if err != nil {
			return nil, err
		}
		res.CompareRange = &RangeOut{From: prev.From, To: prev.To}
		res.CompareRows = prevRows
	}
	return res, nil
}

func columns(plan *Plan) []Column {
	var cols []Column
	if plan.HasTime {
		cols = append(cols, Column{Name: "time", Kind: "time"})
	}
	for _, d := range plan.Dims {
		cols = append(cols, Column{Name: d, Kind: "dimension"})
	}
	for _, m := range plan.Measures {
		cols = append(cols, Column{Name: m.Name, Kind: "measure", Unit: m.Unit})
	}
	return cols
}

// group accumulates leaf values for one (bucket, dims...) key.
type group struct {
	bucket time.Time // zero when the plan has no time grouping
	dims   []string
	vals   map[string]float64 // leaf measure -> value
}

func groupKey(bucket time.Time, dims []string) string {
	var b strings.Builder
	b.WriteString(bucket.UTC().Format(time.RFC3339))
	for _, d := range dims {
		b.WriteByte(0)
		b.WriteString(d)
	}
	return b.String()
}

func ensureGroup(groups map[string]*group, bucket time.Time, dims []string) *group {
	k := groupKey(bucket, dims)
	g, ok := groups[k]
	if !ok {
		g = &group{bucket: bucket, dims: append([]string(nil), dims...), vals: map[string]float64{}}
		groups[k] = g
	}
	return g
}

func (s *Service) run(ctx context.Context, plan *Plan) ([][]any, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	stmts, err := compile(plan, mid.UUID())
	if err != nil {
		return nil, err
	}

	groups := map[string]*group{}
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, st := range stmts {
			if err := scanStmt(ctx, tx, plan, st, groups); err != nil {
				return fmt.Errorf("metrics %s: %w", st.family, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	zeroFill(plan, groups)
	rows := assemble(plan, groups)
	orderRows(plan, rows)
	if len(rows) > plan.Limit {
		rows = rows[:plan.Limit]
	}
	return rows, nil
}

// rawRow is one scanned SQL row before merging.
type rawRow struct {
	bucket  *time.Time
	nullDim bool
	dims    []string
	vals    []float64
}

func scanStmt(ctx context.Context, tx pgx.Tx, plan *Plan, st stmt, groups map[string]*group) error {
	rows, err := tx.Query(ctx, st.sql, st.args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	nDims := len(plan.Dims)
	if st.family == FamDepletion {
		nDims = 0 // depletion supports no dims by construction
	}
	var raws []rawRow
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return err
		}
		r := rawRow{dims: make([]string, nDims), vals: make([]float64, len(st.leaves))}
		i := 0
		if st.hasTime {
			switch t := vals[i].(type) {
			case time.Time:
				tt := t.UTC()
				r.bucket = &tt
			case nil:
				r.bucket = nil // balance base row
			default:
				return fmt.Errorf("unexpected bucket type %T", vals[i])
			}
			i++
		}
		for d := 0; d < nDims; d++ {
			switch v := vals[i].(type) {
			case string:
				r.dims[d] = v
			case nil:
				r.nullDim = true
			default:
				r.dims[d] = fmt.Sprint(v)
			}
			i++
		}
		for v := 0; v < len(st.leaves); v++ {
			r.vals[v] = toFloat(vals[i])
			i++
		}
		raws = append(raws, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if st.balance && plan.HasTime {
		foldBalance(plan, st, raws, groups)
		return nil
	}

	for _, r := range raws {
		if r.nullDim {
			// Snapshot LEFT JOIN artifact: an empty edge yields NULL dims; the
			// zero-filled bucket is produced later.
			continue
		}
		var bucket time.Time
		if st.hasTime {
			if r.bucket == nil {
				continue
			}
			// Snapshot statements always carry the evaluation edge; without a
			// time grouping it is the plan's single edge — fold into the one
			// zero-bucket group so families merge on the same key.
			if plan.HasTime {
				bucket = *r.bucket
			}
		}
		g := ensureGroup(groups, bucket, r.dims)
		for i, m := range st.leaves {
			g.vals[m.Name] = r.vals[i]
		}
	}
	return nil
}

// foldBalance turns per-bucket transfer deltas (NULL bucket = before the first
// edge) into running balances at each bucket label: balance(e_i) = base +
// sum(deltas of buckets before e_i). Strictly-before semantics.
func foldBalance(plan *Plan, st stmt, raws []rawRow, groups map[string]*group) {
	type combo struct {
		dims  []string
		base  []float64
		delta map[time.Time][]float64
	}
	combos := map[string]*combo{}
	for _, r := range raws {
		if r.nullDim {
			continue
		}
		k := strings.Join(r.dims, "\x00")
		c, ok := combos[k]
		if !ok {
			c = &combo{dims: append([]string(nil), r.dims...), base: make([]float64, len(st.leaves)), delta: map[time.Time][]float64{}}
			combos[k] = c
		}
		if r.bucket == nil {
			for i := range c.base {
				c.base[i] += r.vals[i]
			}
			continue
		}
		d, ok := c.delta[*r.bucket]
		if !ok {
			d = make([]float64, len(st.leaves))
		}
		for i := range d {
			d[i] += r.vals[i]
		}
		c.delta[*r.bucket] = d
	}
	for _, c := range combos {
		running := append([]float64(nil), c.base...)
		for _, label := range plan.Buckets {
			g := ensureGroup(groups, label, c.dims)
			for i, m := range st.leaves {
				g.vals[m.Name] = running[i]
			}
			if d, ok := c.delta[label]; ok {
				for i := range running {
					running[i] += d[i]
				}
			}
		}
	}
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	case float64:
		return x
	case float32:
		return float64(x)
	default:
		return 0
	}
}

// zeroFill guarantees every bucket in range appears for every observed
// dimension combo (a gap is data, not an omission).
func zeroFill(plan *Plan, groups map[string]*group) {
	if !plan.HasTime {
		return
	}
	combos := map[string][]string{}
	for _, g := range groups {
		combos[strings.Join(g.dims, "\x00")] = g.dims
	}
	if len(combos) == 0 && len(plan.Dims) == 0 {
		combos[""] = nil
	}
	for _, dims := range combos {
		if dims == nil {
			dims = make([]string, len(plan.Dims))
		}
		for _, b := range plan.Buckets {
			ensureGroup(groups, b, dims)
		}
	}
}

// assemble builds output rows: time, dims..., then measures in request order.
// Ratios divide AFTER aggregation; a zero denominator yields null. Dimension
// combos with zero signal across the whole range (an artifact of measure-level
// FILTERs inside a broader group-by) are dropped — a combo earns a row by
// having at least one non-zero leaf somewhere; time buckets inside a kept
// combo still zero-fill.
func assemble(plan *Plan, groups map[string]*group) [][]any {
	signal := map[string]bool{}
	if len(plan.Dims) > 0 {
		for _, g := range groups {
			key := strings.Join(g.dims, "\x00")
			for _, v := range g.vals {
				if v != 0 {
					signal[key] = true
				}
			}
		}
	}
	rows := make([][]any, 0, len(groups))
	for _, g := range groups {
		if len(plan.Dims) > 0 && !signal[strings.Join(g.dims, "\x00")] {
			continue
		}
		row := make([]any, 0, 1+len(plan.Dims)+len(plan.Measures))
		if plan.HasTime {
			row = append(row, g.bucket.UTC().Format(time.RFC3339))
		}
		for _, d := range g.dims {
			row = append(row, d)
		}
		for _, m := range plan.Measures {
			row = append(row, measureValue(m, g))
		}
		rows = append(rows, row)
	}
	return rows
}

func measureValue(m *Measure, g *group) any {
	if m.Class == ClassRatio {
		num := componentValue(measureByName[m.Num], g)
		den := componentValue(measureByName[m.Den], g)
		if den == 0 {
			return nil
		}
		return num / den
	}
	v := g.vals[m.Name]
	switch m.Unit {
	case "micros", "count":
		return int64(math.Round(v))
	default:
		return v
	}
}

func componentValue(m *Measure, g *group) float64 {
	if m == nil {
		return 0
	}
	if m.Class == ClassRatio {
		den := componentValue(measureByName[m.Den], g)
		if den == 0 {
			return 0
		}
		return componentValue(measureByName[m.Num], g) / den
	}
	return g.vals[m.Name]
}

// orderRows sorts by the user's order terms, then time asc + dims asc for
// stability and determinism.
func orderRows(plan *Plan, rows [][]any) {
	timeIdx := -1
	base := 0
	if plan.HasTime {
		timeIdx = 0
		base = 1
	}
	dimIdx := map[string]int{}
	for i, d := range plan.Dims {
		dimIdx[d] = base + i
	}
	measureIdx := map[string]int{}
	for i, m := range plan.Measures {
		measureIdx[m.Name] = base + len(plan.Dims) + i
	}

	cmpAt := func(a, b []any, idx int, desc bool) int {
		// nil (null ratio) sorts last regardless of direction.
		if a[idx] == nil || b[idx] == nil {
			return compareVals(a[idx], b[idx])
		}
		c := compareVals(a[idx], b[idx])
		if desc {
			return -c
		}
		return c
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		for _, o := range plan.Order {
			var idx int
			if o.Measure != "" {
				idx = measureIdx[o.Measure]
			} else if o.Dimension == "time" {
				idx = timeIdx
			} else {
				idx = dimIdx[o.Dimension]
			}
			if idx < 0 {
				continue
			}
			if c := cmpAt(a, b, idx, o.Dir == "desc"); c != 0 {
				return c < 0
			}
		}
		if timeIdx >= 0 {
			if c := compareVals(a[timeIdx], b[timeIdx]); c != 0 {
				return c < 0
			}
		}
		for _, d := range plan.Dims {
			if c := compareVals(a[dimIdx[d]], b[dimIdx[d]]); c != 0 {
				return c < 0
			}
		}
		return false
	})
}

// compareVals orders strings lexically and numbers numerically; nil sorts last.
func compareVals(a, b any) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return 1
	}
	if b == nil {
		return -1
	}
	switch av := a.(type) {
	case string:
		bv, _ := b.(string)
		return strings.Compare(av, bv)
	default:
		af, bf := toFloat(a), toFloat(b)
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		}
	}
	return 0
}
