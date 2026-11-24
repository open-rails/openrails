package dashboard

import (
	"io/fs"
	"strings"
	"testing"

	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/open-rails/openrails/internal/modules/metrics"
)

func TestDefaultWidgetsValidateForEveryProfile(t *testing.T) {
	for bits := range 16 {
		p := Profile{Subscriptions: bits&1 != 0, CardPayments: bits&2 != 0, Usage: bits&4 != 0, Credits: bits&8 != 0}
		widgets := DefaultWidgets(p)
		if verr := ValidateWidgets(widgets); verr != nil {
			t.Errorf("%+v: %v", p, verr)
		}
		if widgets[0].Query.Measures[0] != "cash" {
			t.Errorf("%+v: the overview starts with %s", p, widgets[0].ID)
		}
	}
}

// The migration that cleans saved layouts names every retired measure.
func TestRetiredMeasuresLeaveSavedLayouts(t *testing.T) {
	names, err := fs.Glob(postgresmigrations.FS, "*_dashboard_retired_measures.up.sql")
	if err != nil || len(names) != 1 {
		t.Fatalf("migration: %v %v", names, err)
	}
	body, err := fs.ReadFile(postgresmigrations.FS, names[0])
	if err != nil {
		t.Fatal(err)
	}
	for name := range metrics.Removed {
		if !strings.Contains(string(body), "'"+name+"'") {
			t.Errorf("saved layouts keep %s", name)
		}
	}
}
