package catalog

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/catalogrules/syntax"
)

// AccessForever spells indefinite access: a one-off purchase that never
// expires. A recurring price's access follows its billing.
const AccessForever = "forever"

// parseAccessDuration reads access_duration: a duration, or forever (Null).
func parseAccessDuration(text Field[string]) (Field[int], error) {
	if text.Null {
		return Field[int]{}, fmt.Errorf("access_duration cannot be null; write %s for access that never expires", AccessForever)
	}
	if strings.TrimSpace(text.Value) == AccessForever {
		return Null[int](), nil
	}
	hours, err := syntax.ParseDurationHours(text.Value)
	if err != nil {
		return Field[int]{}, fmt.Errorf("access_duration: %w", err)
	}
	return Value(hours), nil
}
