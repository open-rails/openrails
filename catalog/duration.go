package catalog

import (
	"time"

	"github.com/open-rails/openrails/pkg/pricing"
)

// ParseDurationSpec parses the catalog duration grammar used by usage windows
// and metered gauge denominators. v1 intentionally supports only whole hours
// and whole days; "once" is accepted by callers that model non-recurring prices.
func ParseDurationSpec(value string) (time.Duration, error) {
	return pricing.ParseDurationSpec(value)
}
