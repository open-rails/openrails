package metrics

import (
	"regexp"
	"slices"
	"strconv"
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
	want := []string{"entitled_customers", "open_findings", "verification_pressure",
		"verification_pressure_age_seconds", "webhook_drift_events", "webhook_rejects", "webhook_silence_age_seconds"}
	if !slices.Equal(ops, want) {
		t.Fatalf("ops measures = %v, want %v", ops, want)
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

// A name the registry does not serve is refused; a planned one says so.
func TestUnknownNames(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	_, verr := ValidateAt(&Query{Measures: []string{"net_revenue", "trials"}, Range: &QueryRange{Last: "30d"},
		Filters: map[string][]string{"attempt_kind": {"renewal"}}}, now)
	if verr == nil {
		t.Fatal("unknown names were accepted")
	}
	got := map[string]FieldError{}
	for _, fe := range verr.Errors {
		got[fe.Param] = fe
	}
	if fe := got["measures[0]"]; fe.Code != "unknown_measure" {
		t.Errorf("net_revenue: %+v", fe)
	}
	if fe := got["measures[1]"]; !strings.Contains(fe.Message, "planned") {
		t.Errorf("trials: %+v", fe)
	}
	if fe := got["filters.attempt_kind"]; fe.Code != "unknown_filter_dimension" {
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

// One name is one dimension: a second definition would shadow the first.
func TestDimensionNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range Dimensions {
		if seen[d.Name] {
			t.Errorf("dimension %q is defined twice", d.Name)
		}
		seen[d.Name] = true
	}
}

// Every family a statement reads names its merchant: the predicate is the
// merchant scope. The event store's read the context's merchant.
func TestEveryFamilyNamesItsMerchant(t *testing.T) {
	for fam, spec := range families {
		if spec.Kind != "depletion" && spec.Kind != "store" && merchantCols[fam] == "" {
			t.Errorf("family %s names no merchant column", fam)
		}
	}
}

// Every public measure compiles, over the range and by time, each statement
// scoped to the merchant and naming every argument it binds: PostgreSQL
// cannot type one it never reads.
func TestEveryMeasureCompiles(t *testing.T) {
	now := time.Date(2026, time.November, 15, 12, 0, 0, 0, time.UTC)
	for _, m := range Measures {
		if m.Internal {
			continue
		}
		for i, q := range []Query{
			{Measures: []string{m.Name}, Range: &QueryRange{Last: "7d"}},
			{Measures: []string{m.Name}, By: []string{"time"}, Grain: "auto", Range: &QueryRange{Last: "30d"}},
		} {
			plan, verr := ValidateAt(&q, now)
			if verr != nil {
				if i == 0 { // a measure may have no time series
					t.Errorf("%s: %v", m.Name, verr)
				}
				continue
			}
			stmts, err := compile(plan, uuid.New(), nil)
			if err != nil {
				t.Errorf("%s by %v: %v", m.Name, q.By, err)
				continue
			}
			for _, st := range stmts {
				if !regexp.MustCompile(`[a-z]+\.merchant_id = \$\d`).MatchString(st.sql) {
					t.Errorf("%s: statement over %s is not scoped to its merchant:\n%s", m.Name, st.family, st.sql)
				}
				for n := range st.args {
					if !regexp.MustCompile(`\$` + strconv.Itoa(n+1) + `\b`).MatchString(st.sql) {
						t.Errorf("%s by %v: statement over %s binds $%d and never reads it:\n%s", m.Name, q.By, st.family, n+1, st.sql)
					}
				}
			}
		}
	}
}
