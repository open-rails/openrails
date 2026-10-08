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

func normalizeEntitlementRow(p Profile, values []*string, priceAccess map[string]*string) error {
	for i, c := range p.Columns {
		if c.Name != "entitlements" && c.Name != "entitlements_snapshot" {
			continue
		}
		if p.Name == "products" && (values[i] == nil || *values[i] == "null") {
			values[i] = new("[]")
		}
		if values[i] == nil {
			continue
		}
		names, hours, err := LegacyEntitlementNames(*values[i])
		if err != nil {
			return err
		}
		values[i] = &names
		if p.Name == "payments" && len(hours) > 0 && value(p, values, "subscription_id") == nil {
			price := value(p, values, "price_id")
			if price != nil {
				if access, known := priceAccess[*price]; known && access == nil {
					for j, field := range p.Columns {
						if field.Name == "legacy_entitlement_hours" {
							raw, err := json.Marshal(hours)
							if err != nil {
								return err
							}
							values[j] = new(string(raw))
						}
					}
				}
			}
		}
	}
	return nil
}
