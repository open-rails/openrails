package money

// A usage item's outcome.
const (
	UsageSucceeded = "succeeded"
	UsageFailed    = "failed"
)

// nilIfEmpty returns nil for "", else a pointer to s (nullable text columns).
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
