package syntax

import (
	"fmt"
	"slices"
	"strings"
)

// ValidateSellVia refuses an empty, unknown or repeated method of known; nil
// is every method.
func ValidateSellVia[M ~string](methods []M, known ...M) error {
	if methods != nil && len(methods) == 0 {
		return fmt.Errorf("sell_via names at least one method; null sells through every method")
	}
	seen := map[M]bool{}
	for _, m := range methods {
		if !slices.Contains(known, m) || seen[m] {
			names := make([]string, len(known))
			for i, k := range known {
				names[i] = string(k)
			}
			return fmt.Errorf("sell_via lists methods once each: %s", strings.Join(names, ", "))
		}
		seen[m] = true
	}
	return nil
}
