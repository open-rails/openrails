package grants

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
)

// DecodeAcceptedEntitlements reads immutable admitted payloads. New operations
// contain opaque names; old operations retain their original object only for
// reproducing accepted fingerprints and historical one-time access windows.
func DecodeAcceptedEntitlements(raw json.RawMessage, retained map[string]*int) ([]string, map[string]*int, error) {
	raw = bytes.TrimSpace(raw)
	var names []string
	if len(raw) == 0 || string(raw) == "null" {
		return nil, retained, nil
	}
	if raw[0] == '{' {
		if err := json.Unmarshal(raw, &retained); err != nil {
			return nil, nil, err
		}
		names = make([]string, 0, len(retained))
		for name := range retained {
			names = append(names, name)
		}
		slices.Sort(names)
	} else if err := json.Unmarshal(raw, &names); err != nil {
		return nil, nil, err
	}
	if retained != nil {
		if len(names) != len(retained) {
			return nil, nil, errors.New("historical entitlement snapshot does not match its names")
		}
		for _, name := range names {
			if _, ok := retained[name]; !ok {
				return nil, nil, errors.New("historical entitlement snapshot does not match its names")
			}
		}
	}
	return names, retained, nil
}

// AcceptedEntitlementValue is used only to reproduce an admitted provider or
// quote fingerprint. Current catalog terms always select the list shape.
func AcceptedEntitlementValue(names []string, historical map[string]*int) any {
	if historical != nil {
		return historical
	}
	return names
}

// HistoricalEntitlementHours extracts the old one-time purchase overrides.
// Subscription grants never used these values to determine access duration.
func HistoricalEntitlementHours(historical map[string]*int) map[string]int {
	var hours map[string]int
	for name, duration := range historical {
		if duration != nil && *duration > 0 {
			if hours == nil {
				hours = map[string]int{}
			}
			hours[name] = *duration
		}
	}
	return hours
}
