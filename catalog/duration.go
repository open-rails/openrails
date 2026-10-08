package catalog

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MaxDurationHours is the largest whole-hour duration that fits time.Duration.
const MaxDurationHours = int((1<<63 - 1) / time.Hour)

// ParseDurationHours parses a positive, whole-number count of hours, days, or
// weeks. Days are exactly 24 hours; calendar months and years are not durations.
func ParseDurationHours(input string) (int, error) {
	parts := strings.Fields(input)
	if len(parts) != 2 {
		return 0, fmt.Errorf("duration must be a positive whole number of hours, days, or weeks")
	}
	count, err := strconv.Atoi(parts[0])
	if err != nil || count <= 0 {
		return 0, fmt.Errorf("duration must be a positive whole number of hours, days, or weeks")
	}
	var multiplier int
	switch strings.ToLower(parts[1]) {
	case "hour", "hours", "h":
		multiplier = 1
	case "day", "days", "d":
		multiplier = 24
	case "week", "weeks", "w":
		multiplier = 7 * 24
	default:
		return 0, fmt.Errorf("unsupported duration unit %q; use hours, days, or weeks", parts[1])
	}
	// Every duration must fit the time arithmetic used to grant access.
	if count > MaxDurationHours/multiplier {
		return 0, fmt.Errorf("duration exceeds %d hours", MaxDurationHours)
	}
	return count * multiplier, nil
}
