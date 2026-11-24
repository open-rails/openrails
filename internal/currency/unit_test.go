package currency

import (
	"errors"
	"testing"
)

func TestMerchantUnits(t *testing.T) {
	gold, err := Unit("GOLD", "Gold coins", "", 0)
	if err != nil || gold.Symbol != "GOLD" || gold.Decimals != 0 || gold.MinorDecimals != 0 || gold.Kind != KindUnit {
		t.Fatalf("GOLD = %+v, %v", gold, err)
	}
	credits, err := Unit("CREDITS2", " Credits ", "✦", 2)
	if err != nil || credits.Name != "Credits" || credits.Decimals != 2 {
		t.Fatalf("CREDITS2 = %+v, %v", credits, err)
	}
	for _, bad := range []struct{ code, name, symbol string }{
		{"USD", "Dollars", ""}, {"USDC", "Coins", ""}, {"GEM", "Gems", ""}, {"gold", "Gold", ""}, {"1GOLD", "Gold", ""},
		{"GOLDCOINS1234", "Gold", ""}, {"GOLD", "", ""}, {"GOLD", "Gold‮", ""}, {"GOLD", "Gold", "$"}, {"GOLD", "Gold", "R$"},
		{"GOLD", "Gold", "€G"}, {"GOLD", "Gold", "123456789"},
	} {
		if _, err := Unit(bad.code, bad.name, bad.symbol, 0); err == nil {
			t.Errorf("Unit(%q, %q, %q) was accepted", bad.code, bad.name, bad.symbol)
		}
	}
	for _, scale := range []int{-1, MaxUnitScale + 1} {
		if _, err := Unit("GOLD", "Gold", "", scale); err == nil {
			t.Errorf("scale %d was accepted", scale)
		}
	}
	if _, err := NewRegistry([]Units{gold, gold}); err == nil {
		t.Error("a unit declared twice was accepted")
	}
	r, err := NewRegistry([]Units{gold, credits})
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := r.Lookup("gold"); !ok || u.Code != "GOLD" {
		t.Errorf("Lookup(gold) = %+v, %v", u, ok)
	}
	if u, err := r.Money("usd"); err != nil || u.Code != "USD" {
		t.Errorf("Money(usd) = %+v, %v", u, err)
	}
	if _, err := r.Money("GOLD"); !errors.Is(err, ErrMerchantUnit) {
		t.Errorf("Money(GOLD) = %v", err)
	}
	if _, err := (Registry{}).Money("GOLD"); !errors.Is(err, ErrUnknown) {
		t.Errorf("another merchant's Money(GOLD) = %v", err)
	}
	if units := r.Units(); len(units) != 2 || units[0].Code != "CREDITS2" || units[1].Code != "GOLD" {
		t.Errorf("Units() = %+v", units)
	}
}
