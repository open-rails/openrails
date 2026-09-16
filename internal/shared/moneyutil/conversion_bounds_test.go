package moneyutil

import (
	"math"
	"testing"
)

func TestRailConversionInt64Bounds(t *testing.T) {
	for _, currency := range CurrencyCodes() {
		got, err := NativeToRailMinor(currency, math.MaxInt64)
		if err != nil {
			t.Fatal(err)
		}
		if got != 922337203685478 {
			t.Fatalf("%s maximum native amount rounded incorrectly: %d", currency, got)
		}
		if _, err := RailMinorToNative(currency, Cents(math.MaxInt64)); err == nil {
			t.Fatalf("%s overflow accepted", currency)
		}
		if _, err := RailMinorToNative(currency, Cents(math.MinInt64)); err == nil {
			t.Fatalf("%s negative overflow accepted", currency)
		}
		for _, minor := range []int64{922337203685477, -922337203685477} {
			native, err := RailMinorToNative(currency, Cents(minor))
			if err != nil {
				t.Fatal(err)
			}
			back, err := NativeToRailMinorExact(currency, native)
			if err != nil {
				t.Fatal(err)
			}
			if int64(back) != minor {
				t.Fatalf("%s conversion lost exactness", currency)
			}
		}
	}
}

// A synthetic currency coarser internally than its rail (shift -2) exercises the
// multiplying direction of NativeToRailMinor, which no registered currency uses.
func TestRailConversionNegativeShiftOverflow(t *testing.T) {
	const code = "NEG"
	currencies[code] = Currency{Code: code, Decimals: 0, MinorDecimals: 2, Kind: "fiat"}
	defer delete(currencies, code)

	if got, err := NativeToRailMinor(code, math.MaxInt64/100); err != nil || int64(got) != math.MaxInt64/100*100 {
		t.Fatalf("largest representable amount: %d, %v", got, err)
	}
	if _, err := NativeToRailMinor(code, math.MaxInt64/100+1); err == nil {
		t.Fatal("ceil converter accepted an overflowing multiplication")
	}
	if _, err := NativeToRailMinorExact(code, math.MinInt64/100-1); err == nil {
		t.Fatal("exact converter accepted a negative overflow")
	}
	if got, err := RailMinorToNative(code, Cents(math.MinInt64)); err != nil || got != math.MinInt64/100 {
		t.Fatalf("inbound division: %d, %v", got, err)
	}
}
