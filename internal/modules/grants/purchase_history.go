package grants

import "time"

// PurchaseWindow is an accepted access interval, separate from recording time.
type PurchaseWindow struct {
	Start time.Time
	End   *time.Time
}

// AccessWindow is a purchase's access: from start for duration hours, or
// indefinitely when the price has no access duration.
func AccessWindow(duration *int, start time.Time) PurchaseWindow {
	window := PurchaseWindow{Start: start}
	if duration != nil && *duration > 0 {
		end := start.Add(time.Duration(*duration) * time.Hour)
		window.End = &end
	}
	return window
}
