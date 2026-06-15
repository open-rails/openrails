package money

import (
	"context"
	"errors"
	"testing"
)

func TestFormatAmount(t *testing.T) {
	cases := []struct {
		minor    int64
		decimals int
		want     string
	}{
		{100, 2, "1.00"}, // gold (#475 spec example)
		{1, 2, "0.01"},
		{12345, 2, "123.45"},
		{5, 0, "5"}, // tickets
		{-100, 2, "-1.00"},
		{1500000, 6, "1.500000"}, // six-decimal precision
		{0, 4, "0.0000"},
	}
	for _, c := range cases {
		if got := FormatAmount(c.minor, c.decimals); got != c.want {
			t.Errorf("FormatAmount(%d,%d)=%q want %q", c.minor, c.decimals, got, c.want)
		}
	}
}

func TestIsQualifiedUnit(t *testing.T) {
	if IsQualifiedUnit("usd") || IsQualifiedUnit("USD") {
		t.Error("built-in must be unqualified")
	}
	if !IsQualifiedUnit("tensorhub/api-credit") {
		t.Error("slug/name must be qualified")
	}
}

func TestRequireBillingCurrencyRejectsCustom(t *testing.T) {
	if err := RequireBillingCurrency("usd"); err != nil {
		t.Errorf("built-in usd must pass billing: %v", err)
	}
	if err := RequireBillingCurrency(""); err != nil {
		t.Errorf("empty (default) must pass billing: %v", err)
	}
	err := RequireBillingCurrency("tensorhub/gold")
	if !errors.Is(err, ErrBillingUnitRequired) {
		t.Errorf("qualified custom unit must be rejected at billing, got %v", err)
	}
}

func TestResolveUnitBuiltin(t *testing.T) {
	s := &MoneyService{}
	d, builtin, err := s.ResolveUnit(context.Background(), "usd")
	if err != nil || !builtin || d != 6 {
		t.Errorf("usd => (6,true,nil), got (%d,%v,%v)", d, builtin, err)
	}
	if _, _, err := s.ResolveUnit(context.Background(), "doge"); err == nil {
		t.Error("unknown built-in must error")
	}
}
