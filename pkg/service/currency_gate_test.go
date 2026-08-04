package service

import "testing"

// or#864 / CUR-8: the service entry-point gate consults the registry now.
// Qualified custom-credit units (#475) are deliberately exempt — they are not
// ISO currencies and never were.
func TestRequireCurrencyConsultsTheRegistry(t *testing.T) {
	for _, ok := range []string{"usd", " USD ", "EUR", "jpy"} {
		if _, err := requireCurrency(ok); err != nil {
			t.Fatalf("%q must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "   ", "XYZ", "USDD", "dollars"} {
		if got, err := requireCurrency(bad); err == nil {
			t.Fatalf("%q must be rejected, got %q", bad, got)
		}
	}
	// A qualified custom-credit unit passes through verbatim (not upper-cased).
	if got, err := requireCurrency("acme/tokens"); err != nil || got != "acme/tokens" {
		t.Fatalf("qualified unit must pass through verbatim, got %q (%v)", got, err)
	}
}
