package booktag

import (
	"testing"
	"time"
)

func TestClassifyAndQuiet(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tag := Tag{ID: "0198f2b0-1111-7000-8000-000000000001", StartedAt: start}
	before, after := start.Add(-time.Hour), start.Add(time.Hour)
	for _, tc := range []struct {
		name, carried string
		created       time.Time
		want          Provenance
		quietLive     bool
		quietNonLive  bool
	}{
		{"this book", " " + "0198F2B0-1111-7000-8000-000000000001", after, Lost, false, false},
		{"this book, no time", tag.ID, time.Time{}, Lost, false, false},
		{"another book", "0198f2b0-2222-7000-8000-000000000002", before, Foreign, true, true},
		{"untagged after tagging began", "", after, Foreign, true, true},
		{"untagged before tagging began", "", before, PreTag, false, true},
		{"untagged, no time", "", time.Time{}, PreTag, false, true},
	} {
		got := tag.Classify(tc.carried, tc.created)
		if got != tc.want || got.Quiet(true) != tc.quietLive || got.Quiet(false) != tc.quietNonLive {
			t.Errorf("%s: %s quiet(live)=%v quiet(non-live)=%v", tc.name, got, got.Quiet(true), got.Quiet(false))
		}
	}
}
