package currency

import "testing"

func TestNativeScalesAndCurrencyKinds(t *testing.T) {
	for _, want := range []Units{
		{Code: "USD", Decimals: 6, MinorDecimals: 2, Kind: "fiat"},
		{Code: "EUR", Decimals: 6, MinorDecimals: 2, Kind: "fiat"},
		{Code: "JPY", Decimals: 4, MinorDecimals: 0, Kind: "fiat"},
		{Code: "SOL", Decimals: 9, MinorDecimals: 9, Kind: "crypto"},
		{Code: "USDC", Decimals: 6, MinorDecimals: 6, Kind: "crypto"},
	} {
		got, ok := Lookup(want.Code)
		if !ok || got != want {
			t.Errorf("Lookup(%s) = %#v, %v; want %#v", want.Code, got, ok, want)
		}
	}
	if got, ok := Lookup(" usdc "); !ok || got.Code != "USDC" {
		t.Fatalf("currency spelling was not normalized: %#v, %v", got, ok)
	}
	for _, unknown := range []string{"", " ", "XYZ", "USDD", "credit:USD"} {
		if _, ok := Lookup(unknown); ok {
			t.Errorf("unregistered currency %q got an implicit scale", unknown)
		}
	}
	first := List()
	first[0].Decimals = 0
	if second := List(); second[0].Decimals != 6 {
		t.Fatal("mutating a returned list changed the registry")
	}
}
