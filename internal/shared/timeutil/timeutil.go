package timeutil

import (
	"fmt"
	"strings"
	"time"
)

func ParseRFC3339UTC(value string) (time.Time, error) {
	return ParseFirstUTC(strings.TrimSpace(value), time.RFC3339)
}

func ParseDateUTC(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("date is empty")
	}
	parsed, err := time.ParseInLocation("2006-01-02", value, time.UTC)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func ParseDateOrRFC3339UTC(value string) (time.Time, error) {
	return ParseFirstUTC(strings.TrimSpace(value), time.RFC3339, "2006-01-02")
}

func ParseFirstUTC(value string, layouts ...string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("time value is empty")
	}

	for _, layout := range layouts {
		parsed, err := time.ParseInLocation(layout, value, time.UTC)
		if err == nil {
			return parsed.UTC(), nil
		}
	}

	return time.Time{}, fmt.Errorf("unable to parse time %q", value)
}
