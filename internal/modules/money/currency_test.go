package money

import "testing"

// TestNativeToRailMinor pins the ONE internal->rail-minor converter (#671):
// internal native units (10^Decimals per major) -> provider minor units
// (10^MinorDecimals per major), ceil. Both call sites (arrears invoice
// collection and auto top-up) must send the SAME provider amount for the same
// internal amount — previously arrears hardcoded /10_000 ceil while topup
// truncated with an assumed 2-decimal scale (JPY 100x divergence).
func TestNativeToRailMinor(t *testing.T) {
	tests := []struct {
		name     string
		currency string
		amount   int64
		want     int64
		wantErr  bool
	}{
		// USD: 6 internal decimals -> cents (2). $19.99 = 19_990_000 -> 1999.
		{name: "usd exact cents", currency: "USD", amount: 19_990_000, want: 1999},
		// Ceil: never under-charge — 1 micro over a cent rounds up.
		{name: "usd sub-cent ceils", currency: "USD", amount: 19_990_001, want: 2000},
		{name: "usd one micro is one cent", currency: "usd", amount: 1, want: 1},
		{name: "usd zero", currency: "USD", amount: 0, want: 0},
		{name: "usd negative clamps", currency: "USD", amount: -5, want: 0},
		// EUR mirrors USD (6 -> 2).
		{name: "eur exact cents", currency: "EUR", amount: 5_000_000, want: 500},
		// JPY: scale-4 internal -> ZERO-decimal rail minor (whole yen).
		// ¥500 = 5_000_000 internal units -> 500 yen (NOT 50_000, NOT 5).
		{name: "jpy whole yen", currency: "JPY", amount: 5_000_000, want: 500},
		{name: "jpy sub-yen ceils", currency: "JPY", amount: 5_000_001, want: 501},
		{name: "jpy one internal unit is one yen", currency: "jpy", amount: 1, want: 1},
		// Unknown currency: error, never a guessed scale.
		{name: "unknown currency errors", currency: "XXX", amount: 100, wantErr: true},
		{name: "blank currency errors", currency: "", amount: 100, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NativeToRailMinor(tt.currency, tt.amount)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if int64(got) != tt.want {
				t.Fatalf("%s %d: expected %d minor units, got %d", tt.currency, tt.amount, tt.want, got)
			}
		})
	}
}

// TestNativeToRailMinorMatchesLegacyArrearsCeilForUSD pins backward
// compatibility on the arrears seam: for 6-decimal currencies the converter is
// exactly the old `(x+9_999)/10_000` ceil (the topup seam now agrees by
// construction — both call sites share this one function).
func TestNativeToRailMinorMatchesLegacyArrearsCeilForUSD(t *testing.T) {
	for _, amt := range []int64{1, 9_999, 10_000, 10_001, 19_990_000} {
		got, err := NativeToRailMinor("USD", amt)
		if err != nil {
			t.Fatal(err)
		}
		want := (amt + 9_999) / 10_000
		if int64(got) != want {
			t.Fatalf("USD %d: expected %d, got %d", amt, want, got)
		}
	}
}
