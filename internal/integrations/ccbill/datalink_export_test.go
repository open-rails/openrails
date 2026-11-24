package ccbill

import (
	"testing"
	"time"
)

func TestDataLinkWindowsAreADayAtMostAndContiguous(t *testing.T) {
	start := time.Date(2026, 11, 1, 6, 30, 0, 500_000_000, time.UTC)
	end := start.Add(50 * time.Hour)
	got := dataLinkWindows(start, end)
	want := [][2]time.Time{
		{start.Truncate(time.Second), start.Truncate(time.Second).Add(24*time.Hour - time.Second)},
		{start.Truncate(time.Second).Add(24 * time.Hour), start.Truncate(time.Second).Add(48*time.Hour - time.Second)},
		{start.Truncate(time.Second).Add(48 * time.Hour), end.Truncate(time.Second).Add(-time.Second)},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d windows, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if !got[i][0].Equal(want[i][0]) || !got[i][1].Equal(want[i][1]) {
			t.Errorf("window %d = %v, want %v", i, got[i], want[i])
		}
	}
	if w := dataLinkWindows(start, start.Add(time.Minute)); len(w) != 1 || w[0][1].Sub(w[0][0]) != time.Minute-time.Second {
		t.Errorf("a minute: %v", w)
	}
	if w := dataLinkWindows(start, start); len(w) != 0 {
		t.Errorf("an empty range asks nothing: %v", w)
	}
}
