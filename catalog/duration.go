package catalog

import (
	"bytes"
	"encoding/json"
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
	count, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || count == 0 {
		return 0, fmt.Errorf("duration must be a positive whole number of hours, days, or weeks")
	}
	var multiplier uint64
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
	if count > uint64(MaxDurationHours)/multiplier {
		return 0, fmt.Errorf("duration exceeds %d hours", MaxDurationHours)
	}
	return int(count * multiplier), nil
}

// UnmarshalJSON accepts readable duration aliases and normalizes them before
// validation and hashing. The Go and serialized forms carry only whole hours.
func (p *ApplyPrice) UnmarshalJSON(raw []byte) error {
	type price ApplyPrice
	var decoded struct {
		price
		AccessDuration  Field[string] `json:"access_duration,omitzero"`
		BillingInterval Field[string] `json:"billing_interval,omitzero"`
		TrialDuration   Field[string] `json:"trial_duration,omitzero"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		text  Field[string]
		hours *Field[int]
	}{
		{"access_duration", decoded.AccessDuration, &decoded.AccessDurationHours},
		{"billing_interval", decoded.BillingInterval, &decoded.BillingIntervalHours},
		{"trial_duration", decoded.TrialDuration, &decoded.TrialDurationHours},
	} {
		if !field.text.Set {
			continue
		}
		if field.hours.Set {
			return fmt.Errorf("%s and %s_hours cannot both be set", field.name, field.name)
		}
		if field.text.Null {
			*field.hours = Null[int]()
			continue
		}
		hours, err := ParseDurationHours(field.text.Value)
		if err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
		*field.hours = Value(hours)
	}
	*p = ApplyPrice(decoded.price)
	return nil
}
