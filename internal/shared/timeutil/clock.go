package timeutil

import "github.com/jonboulle/clockwork"

// FirstClock returns the first non-nil clock, or a real clock when none is.
func FirstClock(clocks ...clockwork.Clock) clockwork.Clock {
	for _, c := range clocks {
		if c != nil {
			return c
		}
	}
	return clockwork.NewRealClock()
}
