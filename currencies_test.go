package openrails

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestCurrencyRegistryWireContract(t *testing.T) {
	raw, err := json.Marshal(CurrencyRegistry{Object: "currencies", Currencies: Currencies()})
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("testdata/wire/currencies.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != strings.TrimSpace(string(fixture)) {
		t.Fatalf("currency registry wire changed:\n%s", raw)
	}
	usd, ok := LookupCurrency("usd")
	if !ok || usd != (CurrencyUnits{Code: "USD", Decimals: 6, MinorDecimals: 2}) || usd.NativeShift() != 4 {
		t.Fatalf("USD lookup: %#v %v", usd, ok)
	}
	if _, ok := LookupCurrency("XYZ"); ok {
		t.Fatal("unregistered currency must not resolve")
	}
}
