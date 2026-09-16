package moneyutil

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"testing"
)

func TestBrowserCurrencyRegistryMatchesEngine(t *testing.T) {
	raw, err := os.ReadFile("../../../web/admin/src/lib/currency-units.json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]int
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{}
	for _, code := range CurrencyCodes() {
		want[code], _ = CurrencyScale(code)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatal("browser currency registry changed; run go run ./scripts/currency-units")
	}
}

func TestFormatAmountUsesRegisteredScale(t *testing.T) {
	const minor3, whole = "TST", "WHL"
	currencies[minor3] = Currency{Code: minor3, Decimals: 6, MinorDecimals: 3, Kind: "fiat"}
	currencies[whole] = Currency{Code: whole, Decimals: 0, MinorDecimals: 0, Kind: "fiat"}
	defer delete(currencies, minor3)
	defer delete(currencies, whole)

	for _, tt := range []struct {
		amount   int64
		currency string
		want     string
	}{
		{19_990_000, "usd", "19.99 USD"},
		{20_000_000, "USD", "20.00 USD"},
		{-1, "EUR", "-0.000001 EUR"},
		{0, "USD", "0.00 USD"},
		{5_000_000, "JPY", "500 JPY"},
		{5_000_001, "JPY", "500.0001 JPY"},
		{math.MaxInt64, "USD", "9223372036854.775807 USD"},
		{math.MinInt64, "JPY", "-922337203685477.5808 JPY"},
		{12_000_000, minor3, "12.000 TST"},
		{12_345_600, minor3, "12.3456 TST"},
		{math.MinInt64, whole, "-9223372036854775808 WHL"},
		{5, "XXX", `5 units of unregistered currency "XXX"`},
	} {
		if got := FormatAmount(tt.amount, tt.currency); got != tt.want {
			t.Errorf("FormatAmount(%d, %q) = %q, want %q", tt.amount, tt.currency, got, tt.want)
		}
	}
}
