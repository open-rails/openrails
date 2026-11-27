package calendarutil

import (
	"testing"
	"time"
	_ "time/tzdata"
)

func TestBuckets(t *testing.T) {
	zone := func(name string) *time.Location {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		return loc
	}
	newYork, kolkata, kathmandu := zone("America/New_York"), zone("Asia/Kolkata"), zone("Asia/Kathmandu")
	utc := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		name      string
		grain     string
		loc       *time.Location
		from, to  time.Time
		n         int
		first     time.Time
		end       time.Time
		localHour string // the local clock of every start, when set
	}{
		{"new york falls back: 25 hours", Hour, newYork, utc("2026-11-01T04:00:00Z"), utc("2026-11-02T05:00:00Z"), 25, utc("2026-11-01T04:00:00Z"), utc("2026-11-02T05:00:00Z"), ""},
		{"new york springs forward: 23 hours", Hour, newYork, utc("2026-03-08T05:00:00Z"), utc("2026-03-09T04:00:00Z"), 23, utc("2026-03-08T05:00:00Z"), utc("2026-03-09T04:00:00Z"), ""},
		{"kolkata hours start on the half hour", Hour, kolkata, utc("2026-11-03T18:45:00Z"), utc("2026-11-03T20:00:00Z"), 2, utc("2026-11-03T18:30:00Z"), utc("2026-11-03T20:30:00Z"), ":00"},
		{"kathmandu days start at quarter past", Day, kathmandu, utc("2026-11-03T18:20:00Z"), utc("2026-11-04T18:16:00Z"), 2, utc("2026-11-03T18:15:00Z"), utc("2026-11-05T18:15:00Z"), "00:00"},
		{"weeks start monday", Week, newYork, utc("2026-11-05T12:00:00Z"), utc("2026-11-10T12:00:00Z"), 2, utc("2026-11-02T05:00:00Z"), utc("2026-11-16T05:00:00Z"), "00:00"},
		{"months across the change", Month, newYork, utc("2026-10-15T12:00:00Z"), utc("2026-11-15T12:00:00Z"), 2, utc("2026-10-01T04:00:00Z"), utc("2026-12-01T05:00:00Z"), "00:00"},
	} {
		starts, end := Buckets(c.from, c.to, c.grain, c.loc, 1000)
		if len(starts) != c.n || !starts[0].Equal(c.first) || !end.Equal(c.end) {
			t.Errorf("%s: %d buckets from %s to %s, want %d from %s to %s", c.name, len(starts), starts[0].Format(time.RFC3339), end.Format(time.RFC3339),
				c.n, c.first.Format(time.RFC3339), c.end.Format(time.RFC3339))
			continue
		}
		seen := map[time.Time]bool{}
		for i, s := range starts {
			if seen[s] {
				t.Errorf("%s: %s twice", c.name, s.Format(time.RFC3339))
			}
			seen[s] = true
			if c.localHour == ":00" && s.In(c.loc).Minute() != 0 || len(c.localHour) == 5 && s.In(c.loc).Format("15:04") != c.localHour {
				t.Errorf("%s: %s starts at %s there", c.name, s.Format(time.RFC3339), s.In(c.loc).Format("15:04"))
			}
			// Every instant of a bucket falls in it.
			next := end
			if i+1 < len(starts) {
				next = starts[i+1]
			}
			for _, at := range []time.Time{s, s.Add(next.Sub(s) / 2), next.Add(-time.Second)} {
				if got := BucketStart(at, c.grain, c.loc); !got.Equal(s) {
					t.Errorf("%s: %s is in %s, not %s", c.name, at.Format(time.RFC3339), got.Format(time.RFC3339), s.Format(time.RFC3339))
				}
			}
		}
	}
	// The repeated hour is two buckets an hour apart, both 01:00 there.
	starts, _ := Buckets(utc("2026-11-01T05:00:00Z"), utc("2026-11-01T07:00:00Z"), Hour, newYork, 1000)
	if len(starts) != 2 || starts[0].In(newYork).Format("15:04") != "01:00" || starts[1].In(newYork).Format("15:04") != "01:00" {
		t.Errorf("the repeated hour: %v", starts)
	}
	// Past max it stops at max+1.
	if starts, _ := Buckets(utc("2026-01-01T00:00:00Z"), utc("2027-01-01T00:00:00Z"), Hour, time.UTC, 744); len(starts) != 745 {
		t.Errorf("a year by hour stops at %d, want 745", len(starts))
	}
}
