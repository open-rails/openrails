package metrics

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRegistry(t *testing.T) {
	var ops []string
	for _, m := range Measures {
		if !m.Internal && m.Group == GroupOps {
			ops = append(ops, m.Name)
		}
	}
	slices.Sort(ops)
	want := []string{"entitled_customers", "nmi_history_authorizations", "open_findings", "verification_pressure",
		"verification_pressure_age_seconds", "webhook_drift_events", "webhook_rejects", "webhook_silence_age_seconds"}
	if !slices.Equal(ops, want) {
		t.Fatalf("ops measures = %v, want %v", ops, want)
	}
	for name, r := range Removed {
		if r.Measure != "" && (measureByName[r.Measure] == nil || measureByName[r.Measure].Internal) {
			t.Errorf("%s is replaced by %q, which is no public measure", name, r.Measure)
		}
	}
	for _, name := range Deferred {
		if measureByName[name] != nil {
			t.Errorf("%s is deferred and live", name)
		}
	}
	rate := measureByName["rebill_rate"]
	if num := measureByName[rate.Num]; !num.Internal || !strings.Contains(num.Expr, "'approved', 'recovered'") {
		t.Errorf("rebill_rate's numerator is %q", num.Expr)
	}
	if slices.Contains(rate.Dims, "outcome") {
		t.Errorf("rebill_rate is groupable by the outcome it slices")
	}
}

func TestRemovedNamesNameTheirReplacement(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	_, verr := ValidateAt(&Query{Measures: []string{"net_revenue", "nmi_history_refused", "nrr"}, Range: &QueryRange{Last: "30d"},
		Filters: map[string][]string{"attempt_kind": {"renewal"}}}, now)
	if verr == nil {
		t.Fatal("removed names were accepted")
	}
	got := map[string]FieldError{}
	for _, fe := range verr.Errors {
		got[fe.Param] = fe
	}
	if fe := got["measures[0]"]; fe.Code != "unknown_measure" || fe.DidYouMean != "cash" {
		t.Errorf("net_revenue: %+v", fe)
	}
	if fe := got["measures[1]"]; fe.DidYouMean != "nmi_history_authorizations" || !strings.Contains(fe.Message, "outcome") {
		t.Errorf("nmi_history_refused: %+v", fe)
	}
	if fe := got["measures[2]"]; !strings.Contains(fe.Message, "planned") {
		t.Errorf("nrr: %+v", fe)
	}
	if fe := got["filters.attempt_kind"]; fe.DidYouMean != "kind" {
		t.Errorf("attempt_kind: %+v", fe)
	}
}

func TestQueryShape(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct{ last, grain string }{{"45d", "day"}, {"46d", "week"}, {"26w", "week"}, {"27w", "month"}, {"1y", "month"}} {
		plan, verr := ValidateAt(&Query{Measures: []string{"rebills"}, By: []string{"time"}, Grain: "auto", Range: &QueryRange{Last: c.last}, Compare: "previous_year"}, now)
		if verr != nil {
			t.Fatalf("%s: %v", c.last, verr)
		}
		if plan.Grain != c.grain || plan.Compare != "previous_year" {
			t.Errorf("%s: grain %s compare %s, want %s", c.last, plan.Grain, plan.Compare, c.grain)
		}
	}
	if _, verr := ValidateAt(&Query{Measures: []string{"rebills", "attempts"}, Range: &QueryRange{Last: "7d"},
		Filters: map[string][]string{"outcome": {"lost"}}}, now); verr == nil || !strings.Contains(verr.Error(), `"attempts"`) {
		t.Errorf("an outcome only rebills has passed on attempts: %v", verr)
	}
	for _, e := range Schema().Examples {
		if _, verr := ValidateAt(&e.Query, now); verr != nil {
			t.Errorf("example %q: %v", e.Intent, verr)
		}
	}
}

// Every family a statement reads names its merchant: the predicate is the
// merchant scope.
func TestEveryFamilyNamesItsMerchant(t *testing.T) {
	for fam, spec := range families {
		if spec.Kind != "depletion" && merchantCols[fam] == "" {
			t.Errorf("family %s names no merchant column", fam)
		}
	}
}

// Every public measure compiles, each statement scoped to the merchant.
func TestEveryMeasureCompiles(t *testing.T) {
	now := time.Date(2026, time.November, 15, 12, 0, 0, 0, time.UTC)
	for _, m := range Measures {
		if m.Internal {
			continue
		}
		q := Query{Measures: []string{m.Name}, Range: &QueryRange{Last: "7d"}}
		plan, verr := ValidateAt(&q, now)
		if verr != nil {
			t.Errorf("%s: %v", m.Name, verr)
			continue
		}
		stmts, err := compile(plan, uuid.New())
		if err != nil {
			t.Errorf("%s: %v", m.Name, err)
			continue
		}
		for _, st := range stmts {
			if !regexp.MustCompile(`[a-z]+\.merchant_id = \$\d`).MatchString(st.sql) {
				t.Errorf("%s: statement over %s is not scoped to its merchant:\n%s", m.Name, st.family, st.sql)
			}
		}
	}
}
