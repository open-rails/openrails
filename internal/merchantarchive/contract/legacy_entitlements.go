package contract

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/open-rails/openrails/catalog"
)

// LegacyEntitlementNames converts only the archived map representation. The
// caller retains positive historical overrides only where they affected an
// already accepted indefinite one-time purchase.
func LegacyEntitlementNames(raw string) (string, map[string]int, error) {
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return raw, nil, nil
	}
	var legacy map[string]*int
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
		return "", nil, err
	}
	names := make([]string, 0, len(legacy))
	var hours map[string]int
	for name, duration := range legacy {
		if strings.TrimSpace(name) == "" {
			return "", nil, fmt.Errorf("legacy entitlement name is blank")
		}
		names = append(names, name)
		if duration != nil {
			if *duration < 0 || *duration > catalog.MaxDurationHours {
				return "", nil, fmt.Errorf("legacy entitlement duration is outside the supported range")
			}
			if *duration > 0 {
				if hours == nil {
					hours = make(map[string]int)
				}
				hours[name] = *duration
			}
		}
	}
	sort.Strings(names)
	encoded, err := json.Marshal(names)
	return string(encoded), hours, err
}

// PrehistoricKeyAddedAt is when a key that predates key history was added:
// the start of time, so older purchases derive it.
const PrehistoricKeyAddedAt = "0001-01-01 00:00:00+00"
