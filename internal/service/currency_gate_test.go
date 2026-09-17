package service

import "testing"

// or#864 / CUR-8: the service entry-point gate consults the registry now.
// Only currencies in the fixed registry are accepted.
func TestRequireCurrencyConsultsTheRegistry(t *testing.T) {
	for _, ok := range []string{"usd", " USD ", "EUR", "jpy"} {
		if _, err := requireCurrency(ok); err != nil {
			t.Fatalf("%q must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "   ", "XYZ", "USDD", "dollars", "acme/tokens", "credit:00000000-0000-0000-0000-000000000000"} {
		if got, err := requireCurrency(bad); err == nil {
			t.Fatalf("%q must be rejected, got %q", bad, got)
		}
	}
}
