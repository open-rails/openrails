package catalog

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseDurationSpec parses the catalog duration grammar used by usage windows
// and metered gauge denominators. v1 intentionally supports only whole hours
// and whole days; "once" is accepted by callers that model non-recurring prices.
func ParseDurationSpec(value string) (time.Duration, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return 0, fmt.Errorf("duration is required")
	}
	unit := value[len(value)-1:]
	n, err := strconv.ParseInt(strings.TrimSpace(value[:len(value)-1]), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("duration %q must be a positive whole h or d value", value)
	}
	switch unit {
	case "h":
		return time.Duration(n) * time.Hour, nil
	case "d":
		return time.Duration(n) * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("duration %q must use h or d", value)
	}
}
