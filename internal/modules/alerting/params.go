package alerting

import (
	"fmt"
	"math"
	"sort"
)

// paramReader validates a rule's raw params (map[string]any from JSON) against a
// template's spec, emitting corrective FieldErrors and building the normalized
// param map (defaults filled) to persist. Unknown keys are an error — a typo'd
// param must fail loudly, never silently no-op (LLM-legibility house style).
type paramReader struct {
	raw     map[string]any
	allowed map[string]bool
	ve      *ValidationError
	out     map[string]any
}

func newParamReader(raw map[string]any, allowed ...string) *paramReader {
	if raw == nil {
		raw = map[string]any{}
	}
	am := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		am[a] = true
	}
	return &paramReader{raw: raw, allowed: am, ve: &ValidationError{}, out: map[string]any{}}
}

// number reads a float param. def==nil + required=false means optional-with-no-default.
func (p *paramReader) number(name string, def, min, max *float64, required bool) float64 {
	v, present := p.raw[name]
	if !present {
		if required {
			p.ve.add(name, "required", fmt.Sprintf("%s is required", name))
			return 0
		}
		if def != nil {
			p.out[name] = *def
			return *def
		}
		return 0
	}
	f, ok := toFloatParam(v)
	if !ok {
		p.ve.add(name, "invalid_type", fmt.Sprintf("%s must be a number, got %T", name, v))
		return 0
	}
	if min != nil && f < *min {
		p.ve.add(name, "out_of_range", fmt.Sprintf("%s must be >= %g", name, *min))
		return f
	}
	if max != nil && f > *max {
		p.ve.add(name, "out_of_range", fmt.Sprintf("%s must be <= %g", name, *max))
		return f
	}
	p.out[name] = f
	return f
}

// integer reads an integer param (JSON number with no fractional part).
func (p *paramReader) integer(name string, def, min, max *float64, required bool) int {
	v, present := p.raw[name]
	if !present {
		if required {
			p.ve.add(name, "required", fmt.Sprintf("%s is required", name))
			return 0
		}
		if def != nil {
			p.out[name] = *def
			return int(*def)
		}
		return 0
	}
	f, ok := toFloatParam(v)
	if !ok || f != math.Trunc(f) {
		p.ve.add(name, "invalid_type", fmt.Sprintf("%s must be a whole number, got %v", name, v))
		return 0
	}
	if min != nil && f < *min {
		p.ve.add(name, "out_of_range", fmt.Sprintf("%s must be >= %g", name, *min))
		return int(f)
	}
	if max != nil && f > *max {
		p.ve.add(name, "out_of_range", fmt.Sprintf("%s must be <= %g", name, *max))
		return int(f)
	}
	p.out[name] = f
	return int(f)
}

// window reads a relative-window string (e.g. 30d, 12w) matching the metrics
// engine's relative-range grammar.
func (p *paramReader) window(name, def string) string {
	v, present := p.raw[name]
	if !present {
		p.out[name] = def
		return def
	}
	s, ok := v.(string)
	if !ok {
		p.ve.add(name, "invalid_type", fmt.Sprintf("%s must be a window string like 30d/12w/6m/1y", name))
		return def
	}
	if !windowRe.MatchString(s) {
		p.ve.add(name, "invalid_window", fmt.Sprintf("%s %q must be a positive count followed by d/w/m/y (e.g. 30d, 12w)", name, s))
		return def
	}
	p.out[name] = s
	return s
}

// finish reports unknown keys and returns the accumulated ValidationError (nil
// when clean).
func (p *paramReader) finish() *ValidationError {
	for k := range p.raw {
		if !p.allowed[k] {
			p.ve.add(k, "unknown_param", fmt.Sprintf("unknown param %q; valid params: %v", k, sortedKeys(p.allowed)), sortedKeys(p.allowed)...)
		}
	}
	return p.ve.orNil()
}

func toFloatParam(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	}
	return 0, false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
