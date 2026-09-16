package money

import "testing"

func TestCurrencyRegistry(t *testing.T) {
	if got := NormalizeCurrency("usd"); got != "USD" {
		t.Fatalf("currency %q", got)
	}
	if d, err := CurrencyDecimals("USD"); err != nil || d != 6 {
		t.Fatalf("scale %d: %v", d, err)
	}
	for _, code := range []string{"", "host-four/gold", "credit:00000000-0000-0000-0000-000000000000", "doge"} {
		if err := RequireBillingCurrency(code); err == nil {
			t.Errorf("accepted unsupported currency %q", code)
		}
	}
}
