package catalog

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// MaxEntitlementKeyBytes bounds an opaque entitlement key in current APIs.
const MaxEntitlementKeyBytes = 256

// MaxProductEntitlements bounds the keys one product grants, so one bundle
// cannot dominate a holder's prefix reads.
const MaxProductEntitlements = 10000

// NormalizeEntitlements validates opaque entitlement names and sorts a copy.
// Names retain their exact spelling. Nil remains nil; an empty list remains an
// explicit empty list, so accepted snapshots can distinguish missing benefits.
func NormalizeEntitlements(entitlements []string) ([]string, error) {
	seen := make(map[string]struct{}, len(entitlements))
	for _, name := range entitlements {
		if len(name) > MaxEntitlementKeyBytes {
			return nil, fmt.Errorf("entitlement names must be at most %d bytes", MaxEntitlementKeyBytes)
		}
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

// NormalizeProductEntitlements is NormalizeEntitlements for a product's keys,
// which also bounds their number.
func NormalizeProductEntitlements(entitlements []string) ([]string, error) {
	if len(entitlements) > MaxProductEntitlements {
		return nil, fmt.Errorf("a product grants at most %d entitlements", MaxProductEntitlements)
	}
	return NormalizeEntitlements(entitlements)
}
