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
// AccessForever spells indefinite access: a one-off purchase that never
// expires. A recurring price's access follows its billing.
const AccessForever = "forever"

// parseAccessDuration reads access_duration: a duration, or forever (Null).
func parseAccessDuration(text Field[string]) (Field[int], error) {
	if text.Null {
		return Field[int]{}, fmt.Errorf("access_duration cannot be null; write %s for access that never expires", AccessForever)
	}
	if strings.TrimSpace(text.Value) == AccessForever {
		return Null[int](), nil
	}
	hours, err := ParseDurationHours(text.Value)
	if err != nil {
		return Field[int]{}, fmt.Errorf("access_duration: %w", err)
	}
	return Value(hours), nil
}

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

// ParseBillingInterval keeps calendar months separate from fixed durations.
// Exactly one result is nonzero. Access and trial durations remain fixed.
func ParseBillingInterval(input string) (hours, months int, err error) {
	parts := strings.Fields(input)
	if len(parts) == 2 {
		unit := strings.ToLower(parts[1])
		if unit == "month" || unit == "months" || unit == "year" || unit == "years" {
			count, parseErr := strconv.Atoi(parts[0])
			multiplier := 1
			if unit == "year" || unit == "years" {
				multiplier = 12
			}
			if parseErr != nil || count < 1 || count > 1200/multiplier {
				return 0, 0, fmt.Errorf("billing interval must be 1 to 1200 calendar months")
			}
			return 0, count * multiplier, nil
		}
	}
	hours, err = ParseDurationHours(input)
	return hours, 0, err
}
