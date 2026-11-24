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
	for bits := range 16 {
		p := Profile{Subscriptions: bits&1 != 0, CardPayments: bits&2 != 0, Usage: bits&4 != 0, Credits: bits&8 != 0}
		widgets := DefaultWidgets(p)
		if verr := ValidateWidgets(widgets); verr != nil {
			t.Errorf("%+v: %v", p, verr)
		}
		first := "cash"
		if p.Subscriptions || Empty(p) {
			first = "mrr"
		}
		if widgets[0].Query.Measures[0] != first {
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
