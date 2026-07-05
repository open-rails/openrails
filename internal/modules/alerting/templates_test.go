package alerting

import "testing"

// Pure param-validation tests (no DB): the template registry compiles + range-
// checks params and fills defaults, emitting corrective errors.

func firstErr(ve *ValidationError) FieldError {
	if ve == nil || len(ve.Errors) == 0 {
		return FieldError{}
	}
	return ve.Errors[0]
}

func TestCompileRule_UnknownTemplate(t *testing.T) {
	_, _, ve := compileRule("nope", nil)
	if ve == nil || firstErr(ve).Code != "unknown_template" {
		t.Fatalf("want unknown_template error, got %v", ve)
	}
}

func TestCompileRule_ChargebackDefaultsAndBounds(t *testing.T) {
	_, norm, ve := compileRule("chargeback_rate_by_rail_account", map[string]any{"threshold": 0.05})
	if ve != nil {
		t.Fatalf("unexpected error: %s", ve.Error())
	}
	if norm["window"] != "30d" {
		t.Fatalf("want default window 30d, got %v", norm["window"])
	}
	if _, _, ve := compileRule("chargeback_rate_by_rail_account", map[string]any{"threshold": 1.5}); ve == nil || firstErr(ve).Code != "out_of_range" {
		t.Fatalf("threshold 1.5 should be out_of_range, got %v", ve)
	}
	if _, _, ve := compileRule("chargeback_rate_by_rail_account", map[string]any{}); ve == nil || firstErr(ve).Code != "required" {
		t.Fatalf("missing threshold should be required, got %v", ve)
	}
}

func TestCompileRule_BadWindowAndUnknownParam(t *testing.T) {
	if _, _, ve := compileRule("chargeback_rate_by_rail_account", map[string]any{"threshold": 0.05, "window": "bad"}); ve == nil || firstErr(ve).Code != "invalid_window" {
		t.Fatalf("bad window should error, got %v", ve)
	}
	_, _, ve := compileRule("chargeback_rate_by_rail_account", map[string]any{"threshold": 0.05, "bogus": 1})
	if ve == nil {
		t.Fatal("unknown param should error")
	}
	found := false
	for _, e := range ve.Errors {
		if e.Code == "unknown_param" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want unknown_param error, got %v", ve.Errors)
	}
}

func TestCompileRule_DepletionRunwayFixed(t *testing.T) {
	_, norm, ve := compileRule("payers_at_depletion_risk", map[string]any{"min_count": 3})
	if ve != nil {
		t.Fatalf("unexpected error: %s", ve.Error())
	}
	if norm["runway_days"] != float64(depletionRiskFixedDays) {
		t.Fatalf("want default runway_days %d, got %v", depletionRiskFixedDays, norm["runway_days"])
	}
	if _, _, ve := compileRule("payers_at_depletion_risk", map[string]any{"min_count": 3, "runway_days": 30}); ve == nil || firstErr(ve).Code != "unsupported_value" {
		t.Fatalf("runway_days!=7 should be unsupported_value, got %v", ve)
	}
}

func TestCompileRule_DunningAndDigestDefaults(t *testing.T) {
	if _, norm, ve := compileRule("dunning_spike", map[string]any{"multiplier": 2}); ve != nil || norm["window"] != "7d" {
		t.Fatalf("dunning defaults: err=%v norm=%v", ve, norm)
	}
	if _, _, ve := compileRule("dunning_spike", map[string]any{"multiplier": 0.5}); ve == nil || firstErr(ve).Code != "out_of_range" {
		t.Fatalf("multiplier 0.5 should be out_of_range, got %v", ve)
	}
	if _, norm, ve := compileRule("payment_methods_expiring", map[string]any{}); ve != nil || norm["days_ahead"] != float64(30) {
		t.Fatalf("digest defaults: err=%v norm=%v", ve, norm)
	}
}

func TestTemplatesRegistryComplete(t *testing.T) {
	want := []string{"chargeback_rate_by_rail_account", "dunning_spike", "payers_at_depletion_risk", "payment_methods_expiring"}
	got := map[string]bool{}
	for _, ti := range Templates() {
		got[ti.Key] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("template %q missing from registry", w)
		}
	}
}
