package metrics

import (
	"testing"
	"time"
	_ "time/tzdata"
)

// Across a midnight clock change each label starts its own date, the first
// instant of it, so no two share a date and a row's date_trunc bucket finds
// its label.
func TestBucketLabelsAcrossMidnightTransitions(t *testing.T) {
	for _, c := range []struct {
		zone, grain, from, to string
		dates                 []string
	}{
		{"Africa/Cairo", "day", "2026-04-22", "2026-04-26", []string{"2026-04-22", "2026-04-23", "2026-04-24", "2026-04-25", "2026-04-26"}},
		{"Africa/Cairo", "day", "2026-10-28", "2026-10-31", []string{"2026-10-28", "2026-10-29", "2026-10-30", "2026-10-31"}},
		{"America/Santiago", "day", "2026-09-04", "2026-09-08", []string{"2026-09-04", "2026-09-05", "2026-09-06", "2026-09-07", "2026-09-08"}},
		{"America/Santiago", "day", "2026-04-03", "2026-04-06", []string{"2026-04-03", "2026-04-04", "2026-04-05", "2026-04-06"}},
		{"America/Santiago", "week", "2026-08-24", "2026-09-20", []string{"2026-08-24", "2026-08-31", "2026-09-07", "2026-09-14"}},
		{"Africa/Cairo", "month", "2026-03-01", "2026-06-30", []string{"2026-03-01", "2026-04-01", "2026-05-01", "2026-06-01"}},
	} {
		zone, err := time.LoadLocation(c.zone)
		if err != nil {
			t.Fatal(err)
		}
		plan, verr := ValidateIn(&Query{Measures: []string{"cash"}, By: []string{"time"}, Grain: c.grain, Range: &QueryRange{From: c.from, To: c.to}},
			time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC), zone)
		if verr != nil {
			t.Fatalf("%s %s: %v", c.zone, c.from, verr)
		}
		var dates []string
		for _, label := range plan.Buckets {
			dates = append(dates, label.In(zone).Format("2006-01-02"))
			if label.Add(-time.Second).In(zone).Format("2006-01-02") == label.In(zone).Format("2006-01-02") {
				t.Errorf("%s %s: %s is not the first instant of its date", c.zone, c.from, label.Format(time.RFC3339))
			}
			if !truncGrain(label.Add(90*time.Minute), c.grain, zone).Equal(label) {
				t.Errorf("%s %s: a row 90 minutes into %s is not bucketed under it", c.zone, c.from, label.Format(time.RFC3339))
			}
		}
		if len(dates) != len(c.dates) {
			t.Fatalf("%s %s %s: labels on %v, want %v", c.zone, c.grain, c.from, dates, c.dates)
		}
		for i := range dates {
			if dates[i] != c.dates[i] {
				t.Fatalf("%s %s %s: labels on %v, want %v", c.zone, c.grain, c.from, dates, c.dates)
			}
		}
		if !plan.From.Equal(plan.Buckets[0]) {
			t.Errorf("%s %s: the range starts at %s, its first label at %s", c.zone, c.from, plan.From.Format(time.RFC3339), plan.Buckets[0].Format(time.RFC3339))
		}
	}
	// A trailing window ending on a transition day starts and ends on dates.
	santiago, _ := time.LoadLocation("America/Santiago")
	from, to, ok := resolveLast("3d", time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC), santiago)
	if !ok || from.In(santiago).Format("2006-01-02 15:04") != "2026-09-04 00:00" || to.In(santiago).Format("2006-01-02 15:04") != "2026-09-07 00:00" {
		t.Errorf("last 3d on 2026-09-06 in Santiago = [%s, %s)", from.In(santiago), to.In(santiago))
	}
}

// The catalogue says which measures read a rollup kept per UTC date.
func TestUTCDayMeasures(t *testing.T) {
	marked := map[string]bool{}
	for _, m := range Schema().Measures {
		marked[m.Name] = m.UTCDays
	}
	for _, name := range []string{"cash", "mrr", "mrr_at_risk", "attempts"} {
		if marked[name] {
			t.Errorf("%s reads instants, not UTC dates", name)
		}
	}
	for _, name := range []string{"usage_units", "usage_charged"} {
		if marked[name] {
			t.Errorf("%s folds 15-minute usage history into the zone's days", name)
		}
	}
	for _, fam := range []Family{FamNetwork, FamWebhookDaily, FamNMIHistory} {
		found := false
		for _, m := range Measures {
			if m.Internal {
				continue
			}
			for _, leaf := range m.components() {
				if leaf.Family == fam {
					found = true
					if !marked[m.Name] {
						t.Errorf("%s reads %s, kept per UTC date", m.Name, fam)
					}
				}
			}
		}
		if !found {
			t.Errorf("no public measure reads %s", fam)
		}
	}
}
