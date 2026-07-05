package alerting

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/open-rails/openrails/internal/modules/metrics"
)

// ParamSpec documents one template parameter for validation + the console form.
type ParamSpec struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"` // number | integer | window
	Required    bool     `json:"required"`
	Default     any      `json:"default,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Description string   `json:"description"`
}

// TemplateInfo is the schema view of a template (Agent B's console renders the
// create/edit form from this).
type TemplateInfo struct {
	Key             string      `json:"key"`
	DisplayName     string      `json:"display_name"`
	Description     string      `json:"description"`
	DefaultSeverity Severity    `json:"default_severity"`
	Digest          bool        `json:"digest"`
	Metric          string      `json:"metric"`
	Params          []ParamSpec `json:"params"`
}

// Outcome is one rule evaluation result.
type Outcome struct {
	// Digest emissions are periodic pushes (payment_methods_expiring), not
	// threshold crossings — the evaluator emits on cadence, never edge-triggers.
	Digest    bool
	Crossed   bool
	Value     float64
	Threshold float64
	Unit      string // ratio | count
	Metric    string
	Dims      map[string]string
	Summary   string
}

// evalContext carries the dependencies a compiled rule needs at evaluation time.
// The metrics service self-pins its own MerchantTx; the merchant is already in
// ctx (the evaluator runs each merchant inside RunInMerchantConn).
type evalContext struct {
	metrics *metrics.Service
	deps    templateEvalDeps
	now     time.Time
}

// templateEvalDeps is the small non-metrics surface a template may need (the
// digest hits payment_methods directly since #733 defers that measure).
type templateEvalDeps interface {
	countPaymentMethodsExpiring(ctx context.Context, now time.Time, daysAhead int) (int64, error)
}

// compiledRule is a rule's typed, validated parameters bound to its evaluation.
type compiledRule interface {
	evaluate(ctx context.Context, ev *evalContext) (Outcome, error)
	metric() string
}

// templateDef is one entry in the in-code registry (metrics-registry style).
type templateDef struct {
	key             string
	displayName     string
	description     string
	metric          string
	defaultSeverity Severity
	digest          bool
	cadence         time.Duration // digest cadence; 0 => every tick
	params          []ParamSpec
	// compile parses+validates raw params, returning the compiled rule and the
	// NORMALIZED params (defaults filled) to persist.
	compile func(raw map[string]any) (compiledRule, map[string]any, *ValidationError)
}

func (t templateDef) info() TemplateInfo {
	return TemplateInfo{
		Key: t.key, DisplayName: t.displayName, Description: t.description,
		DefaultSeverity: t.defaultSeverity, Digest: t.digest, Metric: t.metric, Params: t.params,
	}
}

// DigestCadence is the minimum spacing between digest emissions.
const DigestCadence = 28 * 24 * time.Hour

func f64(v float64) *float64 { return &v }

var templates = func() map[string]templateDef {
	defs := []templateDef{
		{
			key:             "chargeback_rate_by_rail_account",
			displayName:     "Chargeback rate by rail account",
			description:     "Fires when any rail account's chargeback rate over the window crosses the threshold. VAMP monitoring is ~0.9%; warn at a fraction (e.g. 0.0063 ≈ 70% of VAMP).",
			metric:          "chargeback_rate",
			defaultSeverity: SeverityCritical,
			params: []ParamSpec{
				{Name: "threshold", Type: "number", Required: true, Min: f64(0), Max: f64(1),
					Description: "chargeback-rate ceiling as a ratio (0-1); crossing it fires."},
				{Name: "window", Type: "window", Default: "30d",
					Description: "trailing window the rate is measured over (e.g. 30d, 12w)."},
			},
			compile: compileChargeback,
		},
		{
			key:             "dunning_spike",
			displayName:     "Dunning spike",
			description:     "Fires when the current in-dunning (past_due) subscription count exceeds multiplier × its trailing-window baseline — a rail suddenly declining hard.",
			metric:          "subscriptions_in_dunning",
			defaultSeverity: SeverityCritical,
			params: []ParamSpec{
				{Name: "multiplier", Type: "number", Required: true, Min: f64(1),
					Description: "current must exceed multiplier × the baseline average to fire (e.g. 2 = double)."},
				{Name: "window", Type: "window", Default: "7d",
					Description: "trailing window whose daily average is the baseline (e.g. 7d, 4w)."},
			},
			compile: compileDunningSpike,
		},
		{
			key:             "payers_at_depletion_risk",
			displayName:     "Payers at depletion risk",
			description:     "Fires when the count of payers whose prepaid balance covers <= runway_days of burn reaches min_count (top-up revenue / churn-risk moment).",
			metric:          "payers_at_depletion_risk",
			defaultSeverity: SeverityWarning,
			params: []ParamSpec{
				{Name: "min_count", Type: "integer", Required: true, Min: f64(1),
					Description: "fire when at least this many payers are at depletion risk."},
				{Name: "runway_days", Type: "integer", Default: float64(depletionRiskFixedDays), Min: f64(1),
					Description: fmt.Sprintf("days-of-runway horizon; the #733 metrics engine evaluates a FIXED %d-day horizon, so only %d is accepted in v1.", depletionRiskFixedDays, depletionRiskFixedDays)},
			},
			compile: compileDepletion,
		},
		{
			key:             "payment_methods_expiring",
			displayName:     "Payment methods expiring (monthly digest)",
			description:     "Monthly digest of payment methods lapsing within days_ahead — proactive involuntary-churn prevention. A digest push, not a threshold alert.",
			metric:          "payment_methods_expiring",
			defaultSeverity: SeverityWarning,
			digest:          true,
			cadence:         DigestCadence,
			params: []ParamSpec{
				{Name: "days_ahead", Type: "integer", Default: float64(30), Min: f64(1), Max: f64(120),
					Description: "look-ahead horizon: count cards lapsing within this many days."},
			},
			compile: compilePaymentMethodsExpiring,
		},
	}
	m := make(map[string]templateDef, len(defs))
	for _, d := range defs {
		m[d.key] = d
	}
	return m
}()

// depletionRiskFixedDays mirrors the metrics engine's fixed depletion horizon
// (metrics.depletionRiskDays = 7). Kept here so the template param bound is
// legible without importing an unexported metrics const.
const depletionRiskFixedDays = 7

// Templates lists the v1 template registry, key-sorted, for the schema surface.
func Templates() []TemplateInfo {
	out := make([]TemplateInfo, 0, len(templates))
	for _, d := range templates {
		out = append(out, d.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func templateKeys() []string {
	out := make([]string, 0, len(templates))
	for k := range templates {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// compileRule resolves the template, validates params, and returns the compiled
// rule + normalized params (defaults filled) to persist.
func compileRule(template string, params map[string]any) (compiledRule, map[string]any, *ValidationError) {
	def, ok := templates[template]
	if !ok {
		return nil, nil, singleFieldError("template", "unknown_template",
			fmt.Sprintf("unknown template %q; valid templates: %v", template, templateKeys()), templateKeys()...)
	}
	return def.compile(params)
}

// --- compiled implementations ------------------------------------------------

type chargebackRule struct {
	threshold float64
	window    string
}

func (r chargebackRule) metric() string { return "chargeback_rate" }

func (r chargebackRule) evaluate(ctx context.Context, ev *evalContext) (Outcome, error) {
	res, err := runQuery(ctx, ev, metrics.Query{
		Measures: []string{"chargeback_rate"},
		By:       []string{"rail_account"},
		Range:    &metrics.QueryRange{Last: r.window},
	})
	if err != nil {
		return Outcome{}, err
	}
	// Rows: [rail_account, chargeback_rate(ratio|nil)]. Fire when any account
	// crosses; report the worst offender + all currently-offending accounts.
	worst := -1.0
	var worstAcct string
	offending := map[string]string{}
	for _, row := range res.Rows {
		if len(row) < 2 {
			continue
		}
		acct, _ := row[0].(string)
		rate, ok := asFloat(row[len(row)-1])
		if !ok {
			continue
		}
		if rate >= r.threshold {
			offending[acct] = percentf(rate)
		}
		if rate > worst {
			worst, worstAcct = rate, acct
		}
	}
	out := Outcome{
		Crossed: len(offending) > 0, Value: worst, Threshold: r.threshold,
		Unit: "ratio", Metric: "chargeback_rate",
	}
	if out.Crossed {
		out.Dims = map[string]string{"rail_account": worstAcct}
		out.Summary = fmt.Sprintf("chargeback rate on rail account %s is %s (threshold %s)",
			worstAcct, percentf(worst), percentf(r.threshold))
		if len(offending) > 1 {
			out.Summary += fmt.Sprintf("; %d rail accounts over threshold", len(offending))
		}
	}
	return out, nil
}

type dunningSpikeRule struct {
	multiplier float64
	window     string
}

func (r dunningSpikeRule) metric() string { return "subscriptions_in_dunning" }

func (r dunningSpikeRule) evaluate(ctx context.Context, ev *evalContext) (Outcome, error) {
	// Current in-dunning snapshot (now).
	cur, err := runScalar(ctx, ev, metrics.Query{
		Measures: []string{"subscriptions"},
		Filters:  map[string][]string{"status": {"past_due"}},
		Range:    &metrics.QueryRange{Last: "1d"},
	})
	if err != nil {
		return Outcome{}, err
	}
	// Trailing daily baseline over the window.
	series, err := runQuery(ctx, ev, metrics.Query{
		Measures: []string{"subscriptions"},
		Filters:  map[string][]string{"status": {"past_due"}},
		Grain:    "day",
		Range:    &metrics.QueryRange{Last: r.window},
	})
	if err != nil {
		return Outcome{}, err
	}
	var sum float64
	var n int
	for _, row := range series.Rows {
		if len(row) < 2 {
			continue
		}
		if v, ok := asFloat(row[len(row)-1]); ok {
			sum += v
			n++
		}
	}
	baseline := 0.0
	if n > 0 {
		baseline = sum / float64(n)
	}
	threshold := r.multiplier * baseline
	out := Outcome{
		Value: cur, Threshold: threshold, Unit: "count", Metric: "subscriptions_in_dunning",
		// A spike needs a positive baseline (no history => no ratio, no alarm).
		Crossed: baseline > 0 && cur > threshold,
	}
	if out.Crossed {
		out.Summary = fmt.Sprintf("%.0f subscriptions in dunning — %.1f× the %s baseline of %.1f",
			cur, cur/baseline, r.window, baseline)
	}
	return out, nil
}

type depletionRule struct {
	minCount float64
}

func (r depletionRule) metric() string { return "payers_at_depletion_risk" }

func (r depletionRule) evaluate(ctx context.Context, ev *evalContext) (Outcome, error) {
	cur, err := runScalar(ctx, ev, metrics.Query{
		Measures: []string{"payers_at_depletion_risk"},
		Range:    &metrics.QueryRange{Last: "1d"},
	})
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{
		Value: cur, Threshold: r.minCount, Unit: "count", Metric: "payers_at_depletion_risk",
		Crossed: cur >= r.minCount,
	}
	if out.Crossed {
		out.Summary = fmt.Sprintf("%.0f payers at depletion risk (threshold %.0f)", cur, r.minCount)
	}
	return out, nil
}

type paymentMethodsExpiringRule struct {
	daysAhead int
}

func (r paymentMethodsExpiringRule) metric() string { return "payment_methods_expiring" }

func (r paymentMethodsExpiringRule) evaluate(ctx context.Context, ev *evalContext) (Outcome, error) {
	n, err := ev.deps.countPaymentMethodsExpiring(ctx, ev.now, r.daysAhead)
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{
		Digest: true, Value: float64(n), Unit: "count", Metric: "payment_methods_expiring",
		// A digest emits when there is anything to report.
		Crossed: n > 0,
		Summary: fmt.Sprintf("%d payment method(s) expiring within %d days", n, r.daysAhead),
	}
	return out, nil
}

// --- param compilers ---------------------------------------------------------

func compileChargeback(raw map[string]any) (compiledRule, map[string]any, *ValidationError) {
	pr := newParamReader(raw, "threshold", "window")
	threshold := pr.number("threshold", nil, f64(0), f64(1), true)
	window := pr.window("window", "30d")
	if ve := pr.finish(); ve != nil {
		return nil, nil, ve
	}
	r := chargebackRule{threshold: threshold, window: window}
	if ve := validateStructural(metrics.Query{Measures: []string{"chargeback_rate"}, By: []string{"rail_account"}, Range: &metrics.QueryRange{Last: window}}, "window"); ve != nil {
		return nil, nil, ve
	}
	return r, pr.out, nil
}

func compileDunningSpike(raw map[string]any) (compiledRule, map[string]any, *ValidationError) {
	pr := newParamReader(raw, "multiplier", "window")
	mult := pr.number("multiplier", nil, f64(1), nil, true)
	window := pr.window("window", "7d")
	if ve := pr.finish(); ve != nil {
		return nil, nil, ve
	}
	if ve := validateStructural(metrics.Query{Measures: []string{"subscriptions"}, Filters: map[string][]string{"status": {"past_due"}}, Grain: "day", Range: &metrics.QueryRange{Last: window}}, "window"); ve != nil {
		return nil, nil, ve
	}
	return dunningSpikeRule{multiplier: mult, window: window}, pr.out, nil
}

func compileDepletion(raw map[string]any) (compiledRule, map[string]any, *ValidationError) {
	pr := newParamReader(raw, "min_count", "runway_days")
	minCount := pr.integer("min_count", nil, f64(1), nil, true)
	runway := pr.integer("runway_days", f64(depletionRiskFixedDays), f64(1), nil, false)
	if ve := pr.finish(); ve != nil {
		return nil, nil, ve
	}
	if runway != depletionRiskFixedDays {
		return nil, nil, singleFieldError("runway_days", "unsupported_value",
			fmt.Sprintf("the #733 metrics engine evaluates depletion risk at a FIXED %d-day horizon; only %d is supported in v1",
				depletionRiskFixedDays, depletionRiskFixedDays))
	}
	return depletionRule{minCount: float64(minCount)}, pr.out, nil
}

func compilePaymentMethodsExpiring(raw map[string]any) (compiledRule, map[string]any, *ValidationError) {
	pr := newParamReader(raw, "days_ahead")
	days := pr.integer("days_ahead", f64(30), f64(1), f64(120), false)
	if ve := pr.finish(); ve != nil {
		return nil, nil, ve
	}
	return paymentMethodsExpiringRule{daysAhead: days}, pr.out, nil
}

// --- metrics query helpers ---------------------------------------------------

func runQuery(ctx context.Context, ev *evalContext, q metrics.Query) (*metrics.Result, error) {
	plan, verr := metrics.Validate(&q)
	if verr != nil {
		return nil, fmt.Errorf("alerting: build metrics query: %s", verr.Error())
	}
	return ev.metrics.Execute(ctx, plan)
}

// runScalar runs a single-measure, no-dimension query and returns the first
// row's measure value (0 when the result is empty — a genuine zero reading).
func runScalar(ctx context.Context, ev *evalContext, q metrics.Query) (float64, error) {
	res, err := runQuery(ctx, ev, q)
	if err != nil {
		return 0, err
	}
	for _, row := range res.Rows {
		if len(row) == 0 {
			continue
		}
		if v, ok := asFloat(row[len(row)-1]); ok {
			return v, nil
		}
	}
	return 0, nil
}

// validateStructural runs the metrics validator once at compile time so a rule
// with an unparseable window fails create with a corrective param error.
func validateStructural(q metrics.Query, param string) *ValidationError {
	if _, verr := metrics.Validate(&q); verr != nil {
		return singleFieldError(param, "invalid_query", verr.Error())
	}
	return nil
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, false
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case float64:
		return x, true
	case float32:
		return float64(x), true
	}
	return 0, false
}

var windowRe = regexp.MustCompile(`^[1-9][0-9]*[dwmy]$`)
