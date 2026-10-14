// Package calendarutil preserves calendar billing boundaries independently of
// fixed-duration hours.
package calendarutil

import "time"

// AddMonths retains the original day, clamped to the target month's last day.
// Call it with the original anchor and total months to avoid losing a January
// 31 anchor after February's shorter month.
func AddMonths(anchor time.Time, months int) time.Time {
	anchor = anchor.UTC()
	month := time.Date(anchor.Year(), anchor.Month()+time.Month(months), 1, anchor.Hour(), anchor.Minute(), anchor.Second(), anchor.Nanosecond(), time.UTC)
	day := min(anchor.Day(), month.AddDate(0, 1, -1).Day())
	return month.AddDate(0, 0, day-1)
}
