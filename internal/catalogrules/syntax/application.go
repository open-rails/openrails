package syntax

import (
	"fmt"
	"strings"
)

// ValidateApplicationKey checks a catalog document's record key without rewriting it.
func ValidateApplicationKey(key, kind string) error {
	if key == "" || len(key) > 255 || strings.TrimSpace(key) != key {
		return fmt.Errorf("%s key %q must be a nonempty identifier of at most 255 bytes", kind, key)
	}
	return nil
}
