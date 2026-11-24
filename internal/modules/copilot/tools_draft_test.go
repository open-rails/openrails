package copilot

import (
	"testing"
	"time"
)

func TestDefaultEffectiveDateIsTheFirstWholeUTCDayPastTheWindow(t *testing.T) {
	for _, c := range []struct{ now, want string }{
		{"2026-11-23T15:00:00Z", "2026-12-24T00:00:00Z"},
		{"2026-11-23T23:55:00Z", "2026-12-25T00:00:00Z"},
		{"2026-11-23T00:00:00+05:00", "2026-12-23T00:00:00Z"},
	} {
		now, _ := time.Parse(time.RFC3339, c.now)
		if got := defaultEffectiveDate("increase", now).Format(time.RFC3339); got != c.want {
			t.Errorf("an increase drafted at %s takes effect %s, want %s", c.now, got, c.want)
		}
	}
}
