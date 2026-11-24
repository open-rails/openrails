// Package cadence holds the cadence definitions shared by price keys, notices
// and receipts. The analytics buckets have SQL twins in the schema
// (billing_cycle_label, and billing.price_mrr's named cadences).
package cadence

import (
	"fmt"
	"strconv"
	"time"
)

// PriceIntervalLabel is the default price-key interval label. Distinct
// cadences always get distinct labels: <n>h, or <n>d on whole days, with the
// named labels reserved for exactly 168/720/2160/8760 hours.
func PriceIntervalLabel(billingIntervalHours *int) string {
	if billingIntervalHours == nil {
		return "onetime"
	}
	h := *billingIntervalHours
	switch h {
	case 168:
		return "weekly"
	case 720:
		return "monthly"
	case 2160:
		return "quarterly"
	case 8760:
		return "yearly"
	}
	if h > 0 && h%24 == 0 {
		return strconv.Itoa(h/24) + "d"
	}
	return strconv.Itoa(h) + "h"
}

// Same reports whether two prices bill on the same cadence.
func Same(aHours, bHours *int) bool {
	if aHours == nil || bHours == nil {
		return aHours == nil && bHours == nil
	}
	return *aHours == *bHours
}

// FormatRemaining renders a time until an event: whole days from one day up,
// hours below that and minutes below an hour.
func FormatRemaining(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return plural(int(d/(24*time.Hour)), "day")
	case d >= time.Hour:
		return plural(int(d/time.Hour), "hour")
	case d > 0:
		return plural(max(int(d/time.Minute), 1), "minute")
	default:
		return "less than a minute"
	}
}

// FormatDuration renders an exact duration in its largest whole unit: "1 week",
// "30 days", "36 hours", "90 minutes", "45 seconds". Whole hours, days and
// weeks read back through the catalog's duration parser. A fractional second
// keeps Go's notation.
func FormatDuration(d time.Duration) string {
	sign := ""
	if d < 0 {
		sign, d = "-", -d
	}
	for _, u := range []struct {
		size time.Duration
		name string
	}{{7 * 24 * time.Hour, "week"}, {24 * time.Hour, "day"}, {time.Hour, "hour"}, {time.Minute, "minute"}, {time.Second, "second"}} {
		if d >= u.size && d%u.size == 0 {
			return sign + plural(int(d/u.size), u.name)
		}
	}
	if d == 0 {
		return "0 seconds"
	}
	return sign + d.String()
}

// FormatHours is FormatDuration for a whole-hour count.
func FormatHours(hours int) string {
	return FormatDuration(time.Duration(hours) * time.Hour)
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
