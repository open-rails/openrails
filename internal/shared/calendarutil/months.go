// Package calendarutil preserves calendar billing boundaries independently of
// fixed-duration hours.
package calendarutil

import (
	"fmt"
	"time"
)

// AddMonths retains the original day, clamped to the target month's last day.
// Call it with the original anchor and total months to avoid losing a January
// 31 anchor after February's shorter month.
func AddMonths(anchor time.Time, months int) time.Time {
	anchor = anchor.UTC()
	month := time.Date(anchor.Year(), anchor.Month()+time.Month(months), 1, anchor.Hour(), anchor.Minute(), anchor.Second(), anchor.Nanosecond(), time.UTC)
	day := min(anchor.Day(), month.AddDate(0, 1, -1).Day())
	return month.AddDate(0, 0, day-1)
}

// NextBoundary advances a boundary on the original UTC calendar schedule.
// A clamped February date is checked against, but never replaces, the anchor.
func NextBoundary(anchor, boundary time.Time, months int) (time.Time, error) {
	anchor, boundary = anchor.UTC(), boundary.UTC()
	if months < 1 || months > 1200 || anchor.IsZero() || boundary.Before(anchor) {
		return time.Time{}, fmt.Errorf("calendar interval requires an anchor and 1 to 1200 months")
	}
	elapsed := (boundary.Year()-anchor.Year())*12 + int(boundary.Month()-anchor.Month())
	if elapsed%months != 0 || !AddMonths(anchor, elapsed).Equal(boundary) {
		return time.Time{}, fmt.Errorf("boundary does not belong to the original calendar schedule")
	}
	next := AddMonths(anchor, elapsed+months)
	if next.Year() > 9999 || !next.After(boundary) {
		return time.Time{}, fmt.Errorf("calendar boundary is outside the supported timestamp range")
	}
	return next, nil
}
