package moneyutil

import (
	"math"
	"strings"
	"testing"
)

// withCurrency registers a synthetic currency for one test.
func withCurrency(t *testing.T, code string, decimals, minor int) {
	t.Helper()
	currencies[code] = Currency{Code: code, Decimals: decimals, MinorDecimals: minor, Kind: "fiat"}
	t.Cleanup(func() { delete(currencies, code) })
}

// Provider decimals are read at the currency's own minor unit, exactly:
// NMI writes 500 yen as "500.00", which is 500 rail units, never 50 000.
func TestDecimalToRailMinor(t *testing.T) {
	withCurrency(t, "TST", 6, 3)
	for _, tt := range []struct {
		currency, in string
		want         Cents
	}{
		{"USD", "9.99", 999},
		{"usd", " 19.99 ", 1999},
		{"USD", "23", 2300},
		{"USD", "5.5", 550},
		{"USD", ".99", 99},
		{"USD", "9.990000", 999},
		{"USD", "-5.00", -500},
		{"USD", "0.00", 0},
		{"EUR", "1234.56", 123456},
		{"JPY", "500.00", 500},
		{"JPY", "500", 500},
		{"JPY", "-500.00", -500},
		{"KRW", "1500", 1500},
		{"TST", "1.234", 1234},
		{"SOL", "1.5", 1_500_000_000},
		{"USD", "92233720368547758.07", math.MaxInt64},
		{"USD", "-92233720368547758.08", math.MinInt64},
		{"JPY", "9223372036854775807.00", math.MaxInt64},
	} {
		got, err := DecimalToRailMinor(tt.currency, tt.in)
		if err != nil || got != tt.want {
			t.Errorf("DecimalToRailMinor(%s, %q) = %d, %v; want %d", tt.currency, tt.in, got, err, tt.want)
		}
	}
	for _, tt := range []struct{ currency, in string }{
		{"JPY", "500.5"},
		{"KRW", "1500.01"},
		{"USD", "9.999"},
		{"USD", "0.005"},
		{"USD", "5.0010"},
		{"USD", "--1"},
		{"USD", ""},
		{"USD", "   "},
		{"USD", "-"},
		{"USD", "."},
		{"USD", "abc"},
		{"USD", "+1.00"},
		{"USD", "- 1"},
		{"USD", "1.2.3"},
		{"USD", "12,00"},
		{"USD", "1e2"},
		{"USD", "NaN"},
		{"USD", "92233720368547758.08"},
		{"USD", "-92233720368547758.09"},
		{"JPY", "9223372036854775808"},
		{"", "9.99"},
		{"XXX", "9.99"},
	} {
		if got, err := DecimalToRailMinor(tt.currency, tt.in); err == nil {
			t.Errorf("DecimalToRailMinor(%s, %q) = %d; want an error", tt.currency, tt.in, got)
		}
	}
}

// Registered and synthetic scales: 6->2 (USD), 4->0 (JPY), 6->3 (a 3-decimal
// rail minor unit) and 0->2 (internal coarser than the rail).
func TestRailConversions(t *testing.T) {
	withCurrency(t, "TST", 6, 3)
	withCurrency(t, "NEG", 0, 2)
	for _, tt := range []struct {
		currency string
		native   int64
		ceil     Cents // NativeToRailMinor; never under-charges, clamps <= 0
		exact    Cents
		exactErr bool
	}{
		{"USD", 19_990_000, 1999, 1999, false},
		{"usd", 19_990_001, 2000, 0, true},
		{"USD", 1, 1, 0, true},
		{"USD", 0, 0, 0, false},
		{"USD", -19_990_000, 0, -1999, false}, // refund legs keep their sign on the exact path
		{"EUR", 5_000_000, 500, 500, false},
		{"JPY", 5_000_000, 500, 500, false},
		{"JPY", 5_000_001, 501, 0, true},
		{"TST", 19_990_001, 19_991, 0, true},
		{"TST", 19_990_000, 19_990, 19_990, false},
		{"NEG", 7, 700, 700, false},
		{"USD", math.MaxInt64, 922337203685478, 0, true},
	} {
		ceil, err := NativeToRailMinor(tt.currency, tt.native)
		if err != nil || ceil != tt.ceil {
			t.Errorf("NativeToRailMinor(%s, %d) = %d, %v; want %d", tt.currency, tt.native, ceil, err, tt.ceil)
		}
		exact, err := NativeToRailMinorExact(tt.currency, tt.native)
		if (err != nil) != tt.exactErr || exact != tt.exact {
			t.Errorf("NativeToRailMinorExact(%s, %d) = %d, %v; want %d, err=%v", tt.currency, tt.native, exact, err, tt.exact, tt.exactErr)
		}
		if !tt.exactErr {
			back, err := RailMinorToNative(tt.currency, exact)
			if err != nil || back != tt.native {
				t.Errorf("RailMinorToNative(%s, %d) = %d, %v; want %d", tt.currency, exact, back, err, tt.native)
			}
		}
	}
}

func TestRailConversionsRefuseUnknownCurrencyAndOverflow(t *testing.T) {
	for _, code := range []string{"", "   ", "XXX"} {
		if _, err := NativeToRailMinor(code, 100); err == nil {
			t.Errorf("NativeToRailMinor(%q) converted without a registered scale", code)
		}
		if _, err := NativeToRailMinorExact(code, 100); err == nil {
			t.Errorf("NativeToRailMinorExact(%q) converted without a registered scale", code)
		}
		if _, err := RailMinorToNative(code, 100); err == nil {
			t.Errorf("RailMinorToNative(%q) converted without a registered scale", code)
		}
	}
	for _, code := range CurrencyCodes() {
		cur, _ := LookupCurrency(code)
		if cur.NativeShift() <= 0 {
			continue // Atomic token units require no widening.
		}
		for _, minor := range []Cents{math.MaxInt64, math.MinInt64} {
			if _, err := RailMinorToNative(code, minor); err == nil {
				t.Errorf("%s: RailMinorToNative(%d) overflowed silently", code, minor)
			}
		}
	}
	withCurrency(t, "NEG", 0, 2)
	if _, err := NativeToRailMinor("NEG", math.MaxInt64/100+1); err == nil {
		t.Error("ceil converter overflowed silently")
	}
	if _, err := NativeToRailMinorExact("NEG", math.MinInt64/100-1); err == nil {
		t.Error("exact converter overflowed silently")
	}
	if got, err := RailMinorToNative("NEG", math.MinInt64); err != nil || got != math.MinInt64/100 {
		t.Errorf("RailMinorToNative(NEG, MinInt64) = %d, %v", got, err)
	}
}

// CentsToMicros and remaining hardcoded 10^4 conversions are correct only while
// every registered fiat currency shifts by exactly 4 decimals between the
// internal and card-rail scale. Token currencies never use these card paths.
func TestRegisteredFiatCurrenciesShareNativeShift(t *testing.T) {
	if MicrosPerCent != 10_000 || CentsToMicros(1234) != Micros(12_340_000) {
		t.Fatal("cents/micros scale changed")
	}
	for _, code := range CurrencyCodes() {
		cur, _ := LookupCurrency(code)
		if cur.Kind == "fiat" && cur.NativeShift() != 4 {
			t.Errorf("%s native shift %d != 4: route inbound conversions through RailMinorToNative first", code, cur.NativeShift())
		}
	}
}

func TestFormatting(t *testing.T) {
	if got := DescribeNativeScales(); !strings.HasPrefix(got, "AED 1000000, ") || !strings.Contains(got, ", JPY 10000, ") || !strings.Contains(got, ", SOL 1000000000, ") || !strings.HasSuffix(got, ", ZAR 1000000") {
		t.Errorf("DescribeNativeScales() = %q", got)
	}
	withCurrency(t, "TST", 6, 3)
	withCurrency(t, "WHL", 0, 0)
	for _, tt := range []struct{ got, want string }{
		{FormatMicrosDecimal(-12_340_001), "-12.340001"},
		{FormatMicrosDecimal(math.MinInt64), "-9223372036854.775808"},
		{FormatMicrosDecimal(math.MaxInt64), "9223372036854.775807"},
		{FormatCentsDecimal(-1), "-0.01"},
		{FormatCentsDecimal(math.MinInt64), "-92233720368547758.08"},
		{FormatUSD(-12_340_000), "-$12.340000"},
		{FormatUSD(0), "$0.000000"},
		{FormatAmount(19_990_000, "usd"), "19.99 USD"},
		{FormatAmount(20_000_000, "USD"), "20.00 USD"},
		{FormatAmount(-1, "EUR"), "-0.000001 EUR"},
		{FormatAmount(5_000_000, "JPY"), "500 JPY"},
		{FormatAmount(5_000_001, "JPY"), "500.0001 JPY"},
		{FormatAmount(math.MinInt64, "JPY"), "-922337203685477.5808 JPY"},
		{FormatAmount(12_000_000, "TST"), "12.000 TST"},
		{FormatAmount(12_345_600, "TST"), "12.3456 TST"},
		{FormatAmount(math.MinInt64, "WHL"), "-9223372036854775808 WHL"},
		{FormatAmount(5, "XXX"), `5 units of unregistered currency "XXX"`},
	} {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
}
