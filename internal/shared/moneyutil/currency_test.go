package moneyutil

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
		{name: "usd one micro is one cent", currency: "USD", amount: 1, want: 1},
		{name: "usd zero", currency: "USD", amount: 0, want: 0},
		{name: "usd negative clamps", currency: "USD", amount: -5, want: 0},
		// EUR mirrors USD (6 -> 2).
		{name: "eur exact cents", currency: "EUR", amount: 5_000_000, want: 500},
		// JPY: scale-4 internal -> ZERO-decimal rail minor (whole yen).
		// ¥500 = 5_000_000 internal units -> 500 yen (NOT 50_000, NOT 5).
		{name: "jpy whole yen", currency: "JPY", amount: 5_000_000, want: 500},
		{name: "jpy sub-yen ceils", currency: "JPY", amount: 5_000_001, want: 501},
		{name: "jpy one internal unit is one yen", currency: "JPY", amount: 1, want: 1},
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

// TestNativeToRailMinorExact pins the no-rounding sibling: a PRICE that does
// not land on a whole rail minor unit is an error, never a rounding.
func TestNativeToRailMinorExact(t *testing.T) {
	tests := []struct {
		name     string
		currency string
		amount   int64
		want     int64
		wantErr  bool
	}{
		{name: "usd exact cents", currency: "USD", amount: 19_990_000, want: 1999},
		{name: "usd sub-cent errors", currency: "USD", amount: 19_990_001, wantErr: true},
		{name: "usd zero", currency: "USD", amount: 0, want: 0},
		// A negative amount is a refund leg; exactness still applies and the
		// sign survives (the ceil converter clamps, this one must not).
		{name: "usd negative exact", currency: "USD", amount: -19_990_000, want: -1999},
		{name: "eur exact cents", currency: "EUR", amount: 5_000_000, want: 500},
		// JPY: scale-4 internal -> ZERO-decimal rail minor (whole yen).
		{name: "jpy whole yen", currency: "JPY", amount: 5_000_000, want: 500},
		{name: "jpy sub-yen errors", currency: "JPY", amount: 5_000_001, wantErr: true},
		// The whole point of routing every boundary through the registry: an
		// amount whose currency nobody established CANNOT be converted. The
		// deleted MicrosToCentsExact returned 1999 for both of these.
		{name: "blank currency errors", currency: "", amount: 19_990_000, wantErr: true},
		{name: "unregistered currency errors", currency: "XXX", amount: 19_990_000, wantErr: true},
		{name: "whitespace currency errors", currency: "   ", amount: 19_990_000, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NativeToRailMinorExact(tt.currency, tt.amount)
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

// TestRailMinorToNative pins the inbound twin and the round trip.
func TestRailMinorToNative(t *testing.T) {
	for _, tt := range []struct {
		currency string
		minor    Cents
		want     int64
	}{
		{"USD", 1999, 19_990_000},
		{"EUR", 500, 5_000_000},
		{"JPY", 500, 5_000_000}, // 500 yen = 500 * 10^4 internal units
	} {
		got, err := RailMinorToNative(tt.currency, tt.minor)
		if err != nil {
			t.Fatalf("%s: %v", tt.currency, err)
		}
		if got != tt.want {
			t.Fatalf("%s %d minor: expected %d internal, got %d", tt.currency, tt.minor, tt.want, got)
		}
		back, err := NativeToRailMinorExact(tt.currency, got)
		if err != nil || back != tt.minor {
			t.Fatalf("%s round trip: got %d, %v", tt.currency, back, err)
		}
	}
	if _, err := RailMinorToNative("", 100); err == nil {
		t.Fatal("blank currency must not convert")
	}
}

// TestRegistryNativeShiftIsUniform is the tripwire that makes the codebase's
// remaining currency-BLIND conversions honest (or#863). CentsToMicros — 31
// inbound sites — multiplies by a hardcoded 10^4. That is correct today only
// because every registered currency happens to share a native shift of 4
// (USD/EUR 6-2, JPY 4-0). It is not a law; it is a coincidence this test turns
// into a law. Registering a currency with any other shift FAILS here and
// forces the inbound sweep instead of silently mis-scaling 31 call sites.
func TestRegistryNativeShiftIsUniform(t *testing.T) {
	const wantShift = 4 // == log10(MicrosPerCent)
	if MicrosPerCent != 10_000 {
		t.Fatalf("MicrosPerCent changed to %d — re-derive wantShift", MicrosPerCent)
	}
	for _, code := range CurrencyCodes() {
		cur, ok := LookupCurrency(code)
		if !ok {
			t.Fatalf("%s: CurrencyCodes returned an unregistered code", code)
		}
		if cur.Decimals < cur.MinorDecimals {
			t.Fatalf("%s: internal scale (%d) coarser than the rail minor unit (%d)", code, cur.Decimals, cur.MinorDecimals)
		}
		if got := cur.NativeShift(); got != wantShift {
			t.Fatalf("%s has native shift %d, not %d: CentsToMicros and every remaining "+
				"hardcoded 10^4 in the codebase are now WRONG for it. Route the inbound "+
				"sites through RailMinorToNative before registering this currency.", code, got, wantShift)
		}
	}
}

// TestNativeToRailMinorHandlesNonUniformShift proves the converter is genuinely
// scale-driven rather than accidentally correct for a 4-shift registry: a
// synthetic 3-shift currency (a 3-decimal minor unit like KWD/BHD at 6 internal
// decimals) converts by 10^3, where the deleted MicrosToCentsExact would have
// been wrong by exactly 10x.
func TestNativeToRailMinorHandlesNonUniformShift(t *testing.T) {
	const code = "TST"
	currencies[code] = Currency{Code: code, Decimals: 6, MinorDecimals: 3, Kind: "fiat"}
	defer delete(currencies, code)

	got, err := NativeToRailMinorExact(code, 19_990_000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 19_990 {
		t.Fatalf("expected 19_990 minor units (10^3 shift), got %d", got)
	}
	if ceil, err := NativeToRailMinor(code, 19_990_001); err != nil || ceil != 19_991 {
		t.Fatalf("expected ceil 19_991, got %d (%v)", ceil, err)
	}
	if back, err := RailMinorToNative(code, 19_990); err != nil || back != 19_990_000 {
		t.Fatalf("expected 19_990_000 internal, got %d (%v)", back, err)
	}
}
