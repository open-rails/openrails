package timeutil

import "github.com/jonboulle/clockwork"

// FirstClock returns the first non-nil clock from clocks, falling back to a real
// clock when none is supplied. It centralizes the clock-selection helper that was
// previously copy-pasted across the module packages (issue #282 cleanup).
func FirstClock(clocks ...clockwork.Clock) clockwork.Clock {
	for _, c := range clocks {
		if c != nil {
			return c
		}
	}
	return clockwork.NewRealClock()
}
