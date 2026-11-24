package calendarutil

import (
	"testing"
	"time"
	_ "time/tzdata"
)

// Cairo and Santiago change their clocks at midnight: a spring forward skips
// it, so the day starts at the transition; a fall back repeats the hour
// before it.
func TestDayStartAcrossMidnightTransitions(t *testing.T) {
	cairo, err := time.LoadLocation("Africa/Cairo")
	if err != nil {
		t.Fatal(err)
	}
	santiago, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	utc := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		name string
		loc  *time.Location
		day  time.Time
		want time.Time
	}{
		{"cairo before spring forward", cairo, time.Date(2026, 4, 23, 0, 0, 0, 0, time.UTC), utc("2026-04-22T22:00:00Z")},
		{"cairo spring forward", cairo, time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC), utc("2026-04-23T22:00:00Z")},
		{"cairo after spring forward", cairo, time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC), utc("2026-04-24T21:00:00Z")},
		{"cairo fall back", cairo, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC), utc("2026-10-29T22:00:00Z")},
		{"santiago spring forward", santiago, time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC), utc("2026-09-06T04:00:00Z")},
		{"santiago after spring forward", santiago, time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC), utc("2026-09-07T03:00:00Z")},
		{"santiago fall back", santiago, time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC), utc("2026-04-05T04:00:00Z")},
		{"utc", time.UTC, time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC), utc("2026-04-24T00:00:00Z")},
	} {
		got := DayStart(c.day.Year(), c.day.Month(), c.day.Day(), c.loc)
		if !got.Equal(c.want) {
			t.Errorf("%s: DayStart = %s, want %s", c.name, got.UTC().Format(time.RFC3339), c.want.Format(time.RFC3339))
		}
		if y, m, d := got.In(c.loc).Date(); y != c.day.Year() || m != c.day.Month() || d != c.day.Day() {
			t.Errorf("%s: %s is %d-%02d-%02d there", c.name, got.UTC().Format(time.RFC3339), y, m, d)
		}
		// No earlier instant has the date.
		if y, m, d := got.Add(-time.Second).In(c.loc).Date(); y == c.day.Year() && m == c.day.Month() && d == c.day.Day() {
			t.Errorf("%s: the date began before %s", c.name, got.UTC().Format(time.RFC3339))
		}
	}
}
