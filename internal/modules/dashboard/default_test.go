package dashboard

import (
	"io/fs"
	"maps"
	"slices"
	"strings"
	"testing"

	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/open-rails/openrails/internal/modules/metrics"
)

func TestDefaultWidgetsValidateForEveryProfile(t *testing.T) {
	for bits := range 32 {
		p := Profile{Subscriptions: bits&1 != 0, CardPayments: bits&2 != 0, Usage: bits&4 != 0, Credits: bits&8 != 0, Checkouts: bits&16 != 0}
		widgets := DefaultWidgets(p)
		if verr := ValidateWidgets(widgets); verr != nil {
			t.Errorf("%+v: %v", p, verr)
		}
		if widgets[0].Query.Measures[0] != "cash" {
			t.Errorf("%+v: the overview starts with %s", p, widgets[0].ID)
		}
	}
}

// The migrations that clean saved layouts name every retired measure and
// dimension.
func TestRetiredMeasuresLeaveSavedLayouts(t *testing.T) {
	names, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	var cleaning strings.Builder
	for _, name := range names {
		body, err := fs.ReadFile(postgresmigrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "UPDATE billing.dashboard_configs") {
			cleaning.Write(body)
		}
	}
	retired := slices.Collect(maps.Keys(metrics.Removed))
	retired = append(retired, slices.Collect(maps.Keys(metrics.RemovedDimensions))...)
	for _, name := range retired {
		if !strings.Contains(cleaning.String(), "'"+name+"'") {
			t.Errorf("saved layouts keep %s", name)
		}
	}
}

// A funnel draws one count by one ordered dimension, never money or time.
func TestFunnelShape(t *testing.T) {
	funnel := func(q metrics.Query) Widget {
		q.Range = &metrics.QueryRange{Last: "30d"}
		return Widget{ID: "f", Title: "Funnel", Viz: Funnel, Query: q, Grid: Grid{W: 6, H: 4}}
	}
	if verr := ValidateWidgets([]Widget{funnel(metrics.Query{Measures: []string{"checkouts"}, By: []string{"step"}})}); verr != nil {
		t.Fatalf("checkouts by step: %v", verr)
	}
	for name, q := range map[string]metrics.Query{
		"money":       {Measures: []string{"cash"}, By: []string{"cash_kind"}},
		"over time":   {Measures: []string{"checkouts"}, By: []string{"time", "step"}, Grain: "day"},
		"open values": {Measures: []string{"checkouts"}, By: []string{"country"}},
		"two by":      {Measures: []string{"checkouts"}, By: []string{"step", "outcome"}},
	} {
		verr := ValidateWidgets([]Widget{funnel(q)})
		if verr == nil || verr.Errors[0].Code != "funnel_shape" {
			t.Errorf("%s: %v", name, verr)
		}
	}
}
