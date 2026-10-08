package catalog

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// NormalizeEntitlements validates opaque entitlement names and sorts a copy.
// Names retain their exact spelling. Nil remains nil; an empty list remains an
// explicit empty list, so accepted snapshots can distinguish missing benefits.
func NormalizeEntitlements(entitlements []string) ([]string, error) {
	seen := make(map[string]struct{}, len(entitlements))
	for _, name := range entitlements {
		if !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
			return nil, fmt.Errorf("entitlements must contain valid UTF-8 strings without NUL")
		}
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("entitlements must contain nonblank strings")
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("duplicate entitlement %q", name)
		}
		seen[name] = struct{}{}
	}
	result := slices.Clone(entitlements)
	slices.Sort(result)
	return result, nil
}
